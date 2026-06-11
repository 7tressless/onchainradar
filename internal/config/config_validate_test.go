package config

import "testing"

// validConfig returns a Config with every tunable set to a valid value, so a test
// can flip exactly one field and assert Validate's verdict on that field in
// isolation. It mirrors the shipped defaults; keep it in step with the const block.
func validConfig() *Config {
	return &Config{
		ChainID:                     5000,
		ZThreshold:                  4.0,
		WhaleMinMultiple:            25.0,
		BucketMin:                   5,
		BaselineBuckets:             576,
		CooldownMin:                 60,
		PollIntervalSec:             3,
		OutcomeHorizonHours:         6,
		SmartMoneySizeMultiple:      5.0,
		SmartMoneyMinSwaps:          2,
		SmartMoneyWindowHours:       24,
		SmartMoneyThreshold:         50.0,
		SmartMoneyCooldownMin:       240,
		SmartMoneyMinDirectionality: 0.5,
		DiscoverMinLiquidityUSD:     100_000,
		DiscoverMinVol24USD:         250_000,
		BigBorrowMinUSD:             50_000,
		LiquidationMinUSD:           0,
		LSTFlowMinTokens:            10,
		DepegThresholdBps:           50,
		DepegMinSwaps:               5,
		DepegCooldownMin:            60,
	}
}

// TestValidate_LendingFloors checks the lending floor ranges: BigBorrowMinUSD must be
// > 0 and LiquidationMinUSD must be >= 0 (0 = any liquidation fires).
func TestValidate_LendingFloors(t *testing.T) {
	// A fully-valid config (including the lending defaults) passes.
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("the default-valued config should validate, got %v", err)
	}

	// BigBorrowMinUSD <= 0 is rejected.
	for _, bad := range []float64{0, -1} {
		c := validConfig()
		c.BigBorrowMinUSD = bad
		if err := c.Validate(); err == nil {
			t.Errorf("BigBorrowMinUSD=%g should be rejected", bad)
		}
	}
	// A positive BigBorrowMinUSD is accepted.
	c := validConfig()
	c.BigBorrowMinUSD = 1
	if err := c.Validate(); err != nil {
		t.Errorf("BigBorrowMinUSD=1 should be accepted, got %v", err)
	}

	// LiquidationMinUSD < 0 is rejected; 0 and positive are accepted.
	c = validConfig()
	c.LiquidationMinUSD = -1
	if err := c.Validate(); err == nil {
		t.Error("LiquidationMinUSD=-1 should be rejected")
	}
	for _, ok := range []float64{0, 25_000} {
		c := validConfig()
		c.LiquidationMinUSD = ok
		if err := c.Validate(); err != nil {
			t.Errorf("LiquidationMinUSD=%g should be accepted, got %v", ok, err)
		}
	}
}

// TestValidate_DiscoveryFloors checks both discovery gates: DiscoverMinLiquidityUSD and
// DiscoverMinVol24USD must be >= 0 (0 disables that side of the OR-gate).
func TestValidate_DiscoveryFloors(t *testing.T) {
	// A negative reserve floor is rejected.
	c := validConfig()
	c.DiscoverMinLiquidityUSD = -1
	if err := c.Validate(); err == nil {
		t.Error("DiscoverMinLiquidityUSD=-1 should be rejected")
	}
	// A negative volume floor is rejected.
	c = validConfig()
	c.DiscoverMinVol24USD = -1
	if err := c.Validate(); err == nil {
		t.Error("DiscoverMinVol24USD=-1 should be rejected")
	}
	// 0 (disabled side) and positive values are accepted for the volume floor.
	for _, ok := range []float64{0, 250_000, 1_000_000} {
		c := validConfig()
		c.DiscoverMinVol24USD = ok
		if err := c.Validate(); err != nil {
			t.Errorf("DiscoverMinVol24USD=%g should be accepted, got %v", ok, err)
		}
	}
}

// TestValidate_LSTFlowFloor checks the LST-flow floor range: LSTFlowMinTokens must be
// > 0.
func TestValidate_LSTFlowFloor(t *testing.T) {
	// LSTFlowMinTokens <= 0 is rejected.
	for _, bad := range []float64{0, -1} {
		c := validConfig()
		c.LSTFlowMinTokens = bad
		if err := c.Validate(); err == nil {
			t.Errorf("LSTFlowMinTokens=%g should be rejected", bad)
		}
	}
	// A positive floor is accepted.
	for _, ok := range []float64{0.5, 10, 1000} {
		c := validConfig()
		c.LSTFlowMinTokens = ok
		if err := c.Validate(); err != nil {
			t.Errorf("LSTFlowMinTokens=%g should be accepted, got %v", ok, err)
		}
	}
}

// TestValidate_DepegTunables checks the depeg tunable ranges: DepegThresholdBps and
// DepegMinSwaps must be > 0, and DepegCooldownMin must be >= 0 (0 disables the cooldown).
func TestValidate_DepegTunables(t *testing.T) {
	// DepegThresholdBps <= 0 is rejected; positive is accepted.
	for _, bad := range []int{0, -1} {
		c := validConfig()
		c.DepegThresholdBps = bad
		if err := c.Validate(); err == nil {
			t.Errorf("DepegThresholdBps=%d should be rejected", bad)
		}
	}
	for _, ok := range []int{1, 50, 1000} {
		c := validConfig()
		c.DepegThresholdBps = ok
		if err := c.Validate(); err != nil {
			t.Errorf("DepegThresholdBps=%d should be accepted, got %v", ok, err)
		}
	}

	// DepegMinSwaps <= 0 is rejected; positive is accepted.
	for _, bad := range []int{0, -1} {
		c := validConfig()
		c.DepegMinSwaps = bad
		if err := c.Validate(); err == nil {
			t.Errorf("DepegMinSwaps=%d should be rejected", bad)
		}
	}

	// DepegCooldownMin < 0 is rejected; 0 (disabled) and positive are accepted.
	c := validConfig()
	c.DepegCooldownMin = -1
	if err := c.Validate(); err == nil {
		t.Error("DepegCooldownMin=-1 should be rejected")
	}
	for _, ok := range []int{0, 60, 240} {
		c := validConfig()
		c.DepegCooldownMin = ok
		if err := c.Validate(); err != nil {
			t.Errorf("DepegCooldownMin=%d should be accepted, got %v", ok, err)
		}
	}
}
