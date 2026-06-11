// Package outcome grades a matured OCR signal into an explainable report card: once a
// signal's horizon H elapses, it measures what happened in the pool afterward and scores
// the original call across several facets (see ScoreOutcome).
//
// A multi-facet grade is used instead of a single hit-rate because one follow-through
// metric measured only weak lift (~1.3x) on real Mantle data, so an "X% accurate" number
// would overstate the edge. ScoreOutcome is pure and makes no contract calls; AttestPending
// commits the verdict on-chain.
package outcome

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog/log"

	"ocr/internal/attest"
	"ocr/internal/config"
	"ocr/internal/detect"
	"ocr/internal/store"
)

// Facet score targets: the facet value that earns a full 100 sub-score. Fixed here, not
// config, so the grade is comparable across signals and the stored report_hash reproduces
// only when the scale is constant.
const (
	ftTarget  = 2.0 // follow_through (mean post / pre_median): 2.0 = activity doubled.
	escTarget = 3.0 // escalation (max post / pre_median): 3.0 = peaked at 3x baseline.
	dcTarget  = 1.0 // direction (signed post mean / signal value): 1.0 = same flow continued.
)

// Composite weights for the available sub-scores. When DC is absent (a non-netflow signal)
// the other three are renormalized to sum to 1, so the grade is not silently capped at 90.
const (
	weightFT  = 0.40
	weightPER = 0.30
	weightESC = 0.20
	weightDC  = 0.10
)

// minPreBuckets is the floor on pre-window buckets required to trust pre_median; below it
// the baseline is too thin and ScoreOutcome reports ok=false.
const minPreBuckets = 3

// Letter-grade thresholds (inclusive lower bounds), highest first.
const (
	gradeA = 85
	gradeB = 70
	gradeC = 55
	gradeD = 40
)

// Status is a coarse rollup over the numeric grade (the real output): confirmed when
// grade >= gradeC, else transient.
const (
	statusConfirmed = "confirmed"
	statusTransient = "transient"
)

// Facets holds the raw, human-auditable measurements behind the grade. Direction
// is nil for non-netflow signals (it is only meaningful for a signed flow).
type Facets struct {
	FollowThrough float64  `json:"follow_through"`
	Escalation    float64  `json:"escalation"`
	Persistence   float64  `json:"persistence"`
	Direction     *float64 `json:"direction"`
}

// Subs holds the 0..100 sub-scores derived from the facets. DC is nil when
// Direction is nil.
type Subs struct {
	FT  float64  `json:"ft"`
	ESC float64  `json:"esc"`
	PER float64  `json:"per"`
	DC  *float64 `json:"dc"`
}

// Card is the full report card for one matured signal, marshaled to signal_outcomes.card
// and hashed by ReportHash. Field order is the canonical key order (see canonicalCardJSON).
type Card struct {
	SignalID     int64   `json:"signal_id"`
	SignalHash   string  `json:"signal_hash"` // the on-chain signal hash this card grades (0x-hex)
	HorizonHours int     `json:"horizon_hours"`
	Pool         string  `json:"pool"`
	Metric       string  `json:"metric"`
	PreMedian    float64 `json:"pre_median"`
	Facets       Facets  `json:"facets"`
	Subs         Subs    `json:"subs"`
	Grade        int     `json:"grade"`
	Letter       string  `json:"letter"`
	Status       string  `json:"status"`
	Explanation  string  `json:"explanation"`
}

// measuredMetric maps a signal's metric to the non-negative bucket metric used to grade
// follow-through/escalation/persistence, so the facet ratios are well-defined:
//   - netflow0 -> vol0, netflow1 -> vol1 (size via gross volume; signed flow is the DC facet),
//   - whale_swap -> vol0 (a token0-size event; grade token0 volume after it),
//   - swap_count | vol0 | vol1 -> itself.
func measuredMetric(metric string) string {
	switch metric {
	case "netflow0":
		return "vol0"
	case "netflow1":
		return "vol1"
	case "whale_swap":
		return "vol0"
	default:
		return metric // swap_count | vol0 | vol1
	}
}

// bucketMetric extracts a named bucket metric as a float64. Volume/netflow use
// InexactFloat64: the scorer needs magnitude, not exact precision. netflow0/netflow1 return
// the signed value (DC facet only); size metrics are non-negative. An unknown metric yields
// NaN so a caller bug is rejected by the finite guard rather than scored as zero.
func bucketMetric(b store.Bucket, metric string) float64 {
	switch metric {
	case "swap_count":
		return float64(b.SwapCount)
	case "vol0":
		return b.Vol0.InexactFloat64()
	case "vol1":
		return b.Vol1.InexactFloat64()
	case "netflow0":
		return b.Netflow0.InexactFloat64()
	case "netflow1":
		return b.Netflow1.InexactFloat64()
	default:
		return math.NaN()
	}
}

// isNetflow reports whether a metric is a signed net-flow metric (the only
// family for which the DC direction facet is defined).
func isNetflow(metric string) bool {
	return metric == "netflow0" || metric == "netflow1"
}

// clamp constrains x to [0,100]; NaN/Inf map to 0 so a degenerate ratio can
// never produce a non-finite sub-score.
func clamp(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	if x < 0 {
		return 0
	}
	if x > 100 {
		return 100
	}
	return x
}

// signalPayloadLatest reads the signed latest bucket value detect writes for a flow signal,
// the reference for the DC direction facet. A whale payload has no "latest", so it decodes
// to 0 and DC stays absent (correct: whale is not a netflow metric).
type signalPayloadLatest struct {
	Latest float64 `json:"latest"`
}

// ScoreOutcome is the pure, I/O-free scorer: given a signal, its pre/post buckets, and the
// horizon (hours), it computes the graded report card. ok is false (Card zero) when there is
// not enough data to grade (fewer than minPreBuckets pre buckets, empty post window, or
// non-positive pre_median), so the caller skips it this pass.
//
// Facets (mm = the measured metric for sig.Metric):
//
//	pre_median    = median(pre mm)
//	follow_through= mean(post mm) / pre_median
//	escalation    = max(post mm)  / pre_median
//	persistence   = fraction of post buckets with mm >= pre_median
//	direction     = mean(signed post netflow) / payload.latest   (netflow only)
//
// Sub-scores (0..100, clamped), with the facet targets and weights from the consts above:
//
//	FT_sub  = clamp((follow_through-1)/(ftTarget-1)*100)
//	ESC_sub = clamp((escalation-1)/(escTarget-1)*100)
//	PER_sub = persistence*100
//	DC_sub  = clamp(direction/dcTarget*100)               (netflow only)
//
// The grade is their weighted mean (see compositeGrade).
func ScoreOutcome(sig store.Signal, pre, post []store.Bucket, horizonH int) (Card, bool) {
	if len(pre) < minPreBuckets || len(post) == 0 {
		return Card{}, false
	}

	mm := measuredMetric(sig.Metric)

	// pre_median over the measured (non-negative) metric.
	preVals := make([]float64, 0, len(pre))
	for _, b := range pre {
		v := bucketMetric(b, mm)
		if isFinite(v) {
			preVals = append(preVals, v)
		}
	}
	if len(preVals) < minPreBuckets {
		return Card{}, false
	}
	preMedian, _ := detect.MedianAndMAD(preVals)
	if !(preMedian > 0) { // also rejects NaN
		return Card{}, false
	}

	// Post-window stats over the measured metric.
	var (
		sum     float64
		maxVal  float64
		atOrAbv int
		n       int
		haveMax bool
	)
	for _, b := range post {
		v := bucketMetric(b, mm)
		if !isFinite(v) {
			continue
		}
		sum += v
		if !haveMax || v > maxVal {
			maxVal = v
			haveMax = true
		}
		if v >= preMedian {
			atOrAbv++
		}
		n++
	}
	if n == 0 {
		// Post buckets existed but none had a finite measured value: not gradeable.
		return Card{}, false
	}

	followThrough := (sum / float64(n)) / preMedian
	escalation := maxVal / preMedian
	persistence := float64(atOrAbv) / float64(n)

	ftSub := clamp((followThrough - 1) / (ftTarget - 1) * 100)
	escSub := clamp((escalation - 1) / (escTarget - 1) * 100)
	perSub := clamp(persistence * 100)

	facets := Facets{
		FollowThrough: followThrough,
		Escalation:    escalation,
		Persistence:   persistence,
	}
	subs := Subs{FT: ftSub, ESC: escSub, PER: perSub}

	// Direction (DC) facet: signed netflow metrics only, and only when the payload
	// carries a non-zero signed latest to compare against.
	var dirPtr *float64
	var dcPtr *float64
	if isNetflow(sig.Metric) {
		var pl signalPayloadLatest
		if len(sig.Payload) > 0 {
			if err := json.Unmarshal(sig.Payload, &pl); err != nil {
				// A malformed payload is not fatal to grading; log and drop DC.
				log.Warn().Err(err).Int64("signal_id", sig.ID).
					Msg("outcome: decode signal payload for direction; grading without DC")
			}
		}
		if pl.Latest != 0 {
			// Mean of the signed netflow over the post window.
			var sNet float64
			var nNet int
			for _, b := range post {
				v := bucketMetric(b, sig.Metric) // signed netflow0/netflow1
				if !isFinite(v) {
					continue
				}
				sNet += v
				nNet++
			}
			if nNet > 0 {
				postNet := sNet / float64(nNet)
				dir := postNet / pl.Latest // >0 => same direction continued
				if isFinite(dir) {
					dirVal := dir
					dcVal := clamp(dir / dcTarget * 100)
					dirPtr = &dirVal
					dcPtr = &dcVal
				}
			}
		}
	}
	facets.Direction = dirPtr
	subs.DC = dcPtr

	grade := compositeGrade(ftSub, perSub, escSub, dcPtr)
	letter := letterFor(grade)
	status := statusTransient
	if grade >= gradeC {
		status = statusConfirmed
	}

	card := Card{
		SignalID:     sig.ID,
		SignalHash:   signalHashHex(sig),
		HorizonHours: horizonH,
		Pool:         strings.ToLower(sig.Pool),
		Metric:       sig.Metric,
		PreMedian:    preMedian,
		Facets:       facets,
		Subs:         subs,
		Grade:        grade,
		Letter:       letter,
		Status:       status,
		Explanation:  explain(grade, letter, horizonH, facets),
	}
	return card, true
}

// compositeGrade is the weighted mean of the available sub-scores, rounded to an
// int in [0,100]. When dc is nil the FT/PER/ESC weights are renormalized to sum
// to 1 so a non-netflow signal uses the full 0..100 range.
func compositeGrade(ftSub, perSub, escSub float64, dc *float64) int {
	wFT, wPER, wESC := weightFT, weightPER, weightESC
	sum := ftSub*wFT + perSub*wPER + escSub*wESC
	totalW := wFT + wPER + wESC
	if dc != nil {
		sum += (*dc) * weightDC
		totalW += weightDC
	}
	if totalW <= 0 { // unreachable with the constants above; guard anyway
		return 0
	}
	g := int(math.Round(sum / totalW))
	if g < 0 {
		g = 0
	}
	if g > 100 {
		g = 100
	}
	return g
}

// letterFor maps a 0..100 grade to its letter bucket.
func letterFor(grade int) string {
	switch {
	case grade >= gradeA:
		return "A"
	case grade >= gradeB:
		return "B"
	case grade >= gradeC:
		return "C"
	case grade >= gradeD:
		return "D"
	default:
		return "F"
	}
}

// explain builds a deterministic one-liner from the facet values. It is part of the
// hashed card (no wall-clock, no map iteration), so the wording must stay stable.
func explain(grade int, letter string, horizonH int, f Facets) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Grade %s (%d/100): activity held at %.1fx baseline over the next %dh (follow-through)",
		letter, grade, f.FollowThrough, horizonH)
	fmt.Fprintf(&b, ", stayed elevated in %.0f%% of windows (persistence)", f.Persistence*100)
	fmt.Fprintf(&b, ", peaked at %.1fx (escalation)", f.Escalation)
	if f.Direction != nil {
		if *f.Direction >= 0 {
			b.WriteString(", net flow continued same direction")
		} else {
			b.WriteString(", net flow reversed direction")
		}
	}
	b.WriteString(".")
	return b.String()
}

// signalHash32 returns the raw 32-byte on-chain signal hash this card grades, selecting
// the canonical-JSON variant by family exactly as cmd/ocr's attestHash does, so it is the
// same hash the signal was attested with. ok is false when signal_type is outside the
// attestable [1,255] range: the attest path skips such a signal, so there is no hash.
func signalHash32(sig store.Signal) ([32]byte, bool) {
	if sig.SignalType < 1 || sig.SignalType > 255 {
		return [32]byte{}, false
	}
	signalType := uint8(sig.SignalType) //nolint:gosec // range checked above
	score := attest.ScoreFromZ(sig.Zscore)
	if sig.EventRef != "" {
		return attest.SignalHashEvent(sig.Pool, signalType, sig.EventRef, score, sig.BucketTS.Unix()), true
	}
	return attest.SignalHash(sig.Pool, signalType, sig.Metric, score, sig.BucketTS.Unix()), true
}

// signalHashHex returns signalHash32 as a 0x-hex string, empty when the signal_type is
// outside the attestable range, so the card-embedded hash matches the on-chain attestation.
func signalHashHex(sig store.Signal) string {
	h, ok := signalHash32(sig)
	if !ok {
		return ""
	}
	return "0x" + hex.EncodeToString(h[:])
}

// canonicalCardJSON returns the card's encoding/json marshaling. Go emits fields in
// declaration order, escapes strings, and inserts no insignificant whitespace, so the bytes
// are reproducible from the same Card (the determinism ReportHash needs). They are stored
// verbatim in the card TEXT column; jsonb would normalize numbers/whitespace and break the
// re-hash.
func canonicalCardJSON(card Card) ([]byte, error) {
	b, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("outcome: marshal canonical card signal=%d: %w", card.SignalID, err)
	}
	return b, nil
}

// ReportHash returns the 0x-hex keccak256 of a card's canonical JSON encoding.
// It is reproducible from a stored card (re-marshal the card, re-hash) so the
// off-chain verdict can later be committed and independently verified on-chain.
func ReportHash(card Card) (string, error) {
	canonical, err := canonicalCardJSON(card)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(crypto.Keccak256([]byte(canonical))), nil
}

// isFinite reports whether f is a usable real number (not NaN, not ±Inf).
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// Measure finds matured signals with no outcome yet (bucket_ts <= now-H), grades each
// from its pre/post windows, and stores the report card, returning the count inserted.
// A per-signal failure is logged and skipped so one bad signal never aborts the batch;
// only an error fetching the candidate set is fatal. ok=false (not enough data yet) is
// skipped silently and retried a later pass.
func Measure(ctx context.Context, db *store.DB, cfg *config.Config) (int, error) {
	if db == nil || cfg == nil {
		return 0, fmt.Errorf("outcome: nil db or config")
	}
	h := cfg.OutcomeHorizonHours
	if h <= 0 {
		return 0, fmt.Errorf("outcome: non-positive horizon hours %d", h)
	}
	horizon := time.Duration(h) * time.Hour
	// Settle margin: grade only once the full post window (up to T+H) is aggregated. The
	// aggregator lags live blocks, so without this a just-matured signal would freeze with
	// an incomplete, biased-low post window.
	settle := time.Duration(2*cfg.BucketMin) * time.Minute
	now := time.Now().UTC()
	olderThan := now.Add(-horizon - settle)

	// Bound the batch so a large backlog drains over several ticks, not one huge pass.
	const batchLimit = 500
	sigs, err := db.SignalsAwaitingOutcome(ctx, olderThan, batchLimit)
	if err != nil {
		return 0, fmt.Errorf("outcome: list signals awaiting outcome: %w", err)
	}

	measured := 0
	for i := range sigs {
		if err := ctx.Err(); err != nil {
			return measured, fmt.Errorf("outcome: context cancelled: %w", err)
		}
		s := sigs[i]
		t := s.BucketTS.UTC()

		// Pre window [T-H, T); post window (T, T+H]. BucketsBetween is (from, to], so the
		// pre fetch includes the bucket at exactly T; drop it so the spike does not sit in
		// its own baseline (inflating pre_median and biasing every facet down).
		preRaw, perr := db.BucketsBetween(ctx, s.Pool, t.Add(-horizon), t)
		if perr != nil {
			log.Error().Err(perr).Int64("signal_id", s.ID).Msg("outcome: pre buckets; skipping signal")
			continue
		}
		pre := make([]store.Bucket, 0, len(preRaw))
		for _, b := range preRaw {
			if b.TsBucket.Before(t) {
				pre = append(pre, b)
			}
		}
		post, poerr := db.BucketsBetween(ctx, s.Pool, t, t.Add(horizon))
		if poerr != nil {
			log.Error().Err(poerr).Int64("signal_id", s.ID).Msg("outcome: post buckets; skipping signal")
			continue
		}

		card, ok := ScoreOutcome(s, pre, post, h)
		if !ok {
			log.Debug().Int64("signal_id", s.ID).Int("pre", len(pre)).Int("post", len(post)).
				Msg("outcome: not enough data to grade yet; skipping")
			continue
		}

		// Build the canonical bytes once: store verbatim and hash, so the stored card
		// re-hashes to report_hash.
		canonical, herr := canonicalCardJSON(card)
		if herr != nil {
			log.Error().Err(herr).Int64("signal_id", s.ID).Msg("outcome: build canonical card; skipping signal")
			continue
		}
		reportHash := "0x" + hex.EncodeToString(crypto.Keccak256(canonical))

		inserted, ierr := db.InsertOutcome(ctx, store.Outcome{
			SignalID:     card.SignalID,
			HorizonHours: card.HorizonHours,
			Grade:        card.Grade,
			Letter:       card.Letter,
			Status:       card.Status,
			ReportHash:   reportHash,
			Card:         json.RawMessage(canonical),
		})
		if ierr != nil {
			log.Error().Err(ierr).Int64("signal_id", s.ID).Msg("outcome: insert outcome; skipping signal")
			continue
		}
		if !inserted {
			// Raced with another pass (or already measured): not an error.
			log.Debug().Int64("signal_id", s.ID).Msg("outcome: already measured, skipping")
			continue
		}
		measured++
		log.Info().
			Int64("signal_id", s.ID).
			Str("pool", card.Pool).
			Str("metric", card.Metric).
			Int("grade", card.Grade).
			Str("letter", card.Letter).
			Str("report_hash", reportHash).
			Msg("outcome: report card measured")
	}

	log.Info().Int("measured", measured).Int("scanned", len(sigs)).Msg("outcome: measure pass complete")
	return measured, nil
}

// outcomeAttestBatch bounds how many pending outcomes one AttestPending pass submits, so
// a large backlog drains over several ticks rather than one unbounded money-path sweep.
const outcomeAttestBatch = 200

// attestOutcomeTimeout bounds the per-outcome attest RPC round-trips (nonce + gas + send)
// so a hung node cannot stall the pass. Matches the signal hot path's attest budget.
const attestOutcomeTimeout = 30 * time.Second

// attestWriteAttempts / attestWriteBackoff bound the retry on the DB write that persists
// an outcome's attest tx hash. The broadcast is irreversible, so retrying the cheap write
// avoids leaving attest_tx empty and re-submitting a duplicate next pass.
const (
	attestWriteAttempts = 3
	attestWriteBackoff  = 200 * time.Millisecond
)

// persistOutcomeAttestTx records an already-broadcast outcome attest tx hash, retrying a
// transient failure with a short ctx-aware backoff (see attestWriteAttempts). On final
// failure it returns the last error, never swallowed.
func persistOutcomeAttestTx(ctx context.Context, write func(context.Context) error) error {
	var err error
	for attempt := 1; attempt <= attestWriteAttempts; attempt++ {
		if err = write(ctx); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if attempt < attestWriteAttempts {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(attestWriteBackoff):
			}
		}
	}
	return err
}

// gradeMax is the uint8 ceiling guarding the int->uint8 narrowing of a grade before it is
// committed on-chain. compositeGrade clamps to [0,100], so a value outside [0,255] is a
// corruption bug: reject it loudly rather than wrap.
const gradeMax = 255

// OutcomeAttestorClient is the minimal on-chain interface AttestPending needs: submit one
// outcome attestation, returning the tx hash without waiting for the receipt.
// *attest.OutcomeAttestor satisfies it.
type OutcomeAttestorClient interface {
	AttestOutcome(ctx context.Context, signalHash [32]byte, grade uint8, reportHash [32]byte) (string, error)
}

// parseHash32 decodes a 0x-prefixed (or bare) 32-byte hex string into a [32]byte. It
// rejects a wrong length or non-hex input rather than truncating/zero-padding, so a
// malformed stored hash can never be committed on-chain as a different value.
func parseHash32(s string) ([32]byte, error) {
	var h [32]byte
	raw := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	b, err := hex.DecodeString(raw)
	if err != nil {
		return h, fmt.Errorf("decode hash %q: %w", s, err)
	}
	if len(b) != 32 {
		return h, fmt.Errorf("hash %q is %d bytes, want 32", s, len(b))
	}
	copy(h[:], b)
	return h, nil
}

// AttestPending commits matured-but-unattested outcome report cards on-chain: it pulls a
// bounded batch of outcomes whose attest_tx is empty, derives the same signalHash the
// signal was attested with (so OutcomeRecorded links back to SignalAttested), and submits
// attestOutcome() for each, returning the count attested.
//
// Idempotent + best-effort, mirroring the signal attest path:
//   - OutcomesAwaitingAttest excludes any outcome that already has an attest_tx, so a
//     re-run never re-attests. The tx hash is persisted right after a successful send, so
//     a crash leaves a recoverable breadcrumb.
//   - A per-outcome failure is logged and skipped so one bad row never aborts the batch;
//     only an error fetching the candidate set is fatal.
//   - A no-op (0, nil) when attestor is nil.
func AttestPending(ctx context.Context, db *store.DB, attestor OutcomeAttestorClient) (int, error) {
	if db == nil {
		return 0, fmt.Errorf("outcome: nil db")
	}
	if attestor == nil {
		// On-chain outcome attestation disabled: nothing to do, not an error.
		return 0, nil
	}

	rows, err := db.OutcomesAwaitingAttest(ctx, outcomeAttestBatch)
	if err != nil {
		return 0, fmt.Errorf("outcome: list outcomes awaiting attest: %w", err)
	}

	attested := 0
	for i := range rows {
		if err := ctx.Err(); err != nil {
			return attested, fmt.Errorf("outcome: context cancelled: %w", err)
		}
		r := rows[i]
		sig := r.Signal

		// Derive the on-chain signal hash exactly as the signal was attested with. A
		// signal_type outside the attestable range was never attested, so skip it.
		sigHash, ok := signalHash32(sig)
		if !ok {
			log.Error().Int64("signal_id", sig.ID).Int16("signal_type", sig.SignalType).
				Msg("outcome: signal_type out of attestable range [1,255]; skipping outcome attest")
			continue
		}

		// Guard the int->uint8 narrowing before the grade goes on-chain (see gradeMax).
		if r.Grade < 0 || r.Grade > gradeMax {
			log.Error().Int64("signal_id", sig.ID).Int("grade", r.Grade).
				Msg("outcome: grade out of uint8 range [0,255]; skipping outcome attest")
			continue
		}
		grade := uint8(r.Grade) //nolint:gosec // range checked above

		reportHash, perr := parseHash32(r.ReportHash)
		if perr != nil {
			log.Error().Err(perr).Int64("signal_id", sig.ID).
				Msg("outcome: malformed report_hash; skipping outcome attest")
			continue
		}

		// Bound the attest RPC round-trips so a hung node cannot stall the pass.
		actx, acancel := context.WithTimeout(ctx, attestOutcomeTimeout)
		tx, aerr := attestor.AttestOutcome(actx, sigHash, grade, reportHash)
		acancel()
		if aerr != nil {
			log.Error().Err(aerr).Int64("signal_id", sig.ID).Msg("outcome: attest outcome failed; continuing")
			continue
		}

		// Persist the tx hash immediately so a crash never loses the breadcrumb. The
		// tx is already on-chain, so retry the cheap write a few times.
		if uerr := persistOutcomeAttestTx(ctx, func(c context.Context) error {
			return db.UpdateOutcomeAttest(c, sig.ID, tx)
		}); uerr != nil {
			log.Error().Err(uerr).Int64("signal_id", sig.ID).Str("tx", tx).
				Msg("outcome: persist outcome attest tx failed after retries")
			// The tx is on-chain; not a failure, but the empty attest_tx lets a later pass
			// re-emit. OutcomeRecorded is keyed on the same signalHash, so a duplicate is a
			// harmless identical emit (consumers dedup, earliest ts wins).
		}
		attested++
		log.Info().
			Int64("signal_id", sig.ID).
			Str("pool", strings.ToLower(sig.Pool)).
			Int("grade", r.Grade).
			Str("report_hash", r.ReportHash).
			Str("tx", tx).
			Msg("outcome: outcome attested on-chain")
	}

	log.Info().Int("attested", attested).Int("scanned", len(rows)).Msg("outcome: attest pass complete")
	return attested, nil
}
