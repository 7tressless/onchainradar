// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

/// @title OutcomeAttestor
/// @notice Events-only on-chain record of OCR (OnChain Radar) signal outcomes: the
///         matured, graded report card for each anomaly the agent previously called.
///         After a signal's horizon elapses, OCR measures what actually happened in
///         the pool and grades the original call 0..100; this contract publishes that
///         verdict as a tamper-proof, timestamped event so the agent's hit-rate is
///         independently verifiable from logs alone. The contract stores no outcome
///         data, only access control.
/// @dev    Sidecar to {SignalAttestor}: a separate contract that does not touch the
///         signal attestor or its live track record. Each OutcomeRecorded links back
///         to the original SignalAttested record via `signalHash` (the same keccak256
///         the signal was attested with), so a consumer can join the call (signal) to
///         its graded result (outcome) purely on-chain.
///
///         Deliberately storage-free for outcomes: emitting an event costs almost no
///         gas, and the dashboard reads the entire history straight from the log
///         topics. Only `owner`, `pendingOwner`, and `agent` are persisted (for
///         access control and rotation). This mirrors {SignalAttestor}.
///
///         Dedup policy: attestOutcome() emits unconditionally, with no on-chain
///         signalHash-seen guard. Duplicate suppression is the caller's responsibility:
///         the Go attestor never calls attestOutcome() for an outcome that already has
///         an attest_tx in Postgres (one outcome row per signal_id; see migration
///         0007). Any consumer reading logs should dedup by signalHash (earliest ts
///         wins) when reconstructing the canonical outcome record.
///
///         Consumer filter invariant: OutcomeRecorded does not include msg.sender, and
///         every field (signalHash, grade, reportHash) is caller-supplied input.
///         The only thing tying a log to this agent is the emitting contract address.
///         Any eth_getLogs / FilterLogs consumer reconstructing the outcome record
///         must scope the query to this contract's address; matching on topic0 alone
///         would let an unrelated contract emitting the same event mix its outcomes
///         into the agent's history. Always set FilterQuery.Addresses =
///         [deployedOutcomeAttestorAddress] and never match by topic0 only.
contract OutcomeAttestor {
    /// @notice Emitted once per measured signal outcome.
    /// @param signalHash keccak256 of the canonical signal JSON; the same hash the
    ///        signal was attested with on {SignalAttestor}, which links the graded
    ///        outcome back to the original call (indexed for join/lookup).
    /// @param grade      Composite outcome score 0..100 (higher = the call followed
    ///        through better). Not capped on-chain beyond the uint8 width; the caller
    ///        is responsible for keeping it in [0,100].
    /// @param reportHash keccak256 of the canonical off-chain report-card JSON
    ///        (off-chain integrity key; the card re-hashes to this value).
    /// @param ts         Block timestamp at attestation (seconds since epoch).
    event OutcomeRecorded(
        bytes32 indexed signalHash,
        uint8 grade,
        bytes32 reportHash,
        uint64 ts
    );

    /// @notice Emitted when the agent address is rotated by the owner.
    event AgentUpdated(address indexed previousAgent, address indexed newAgent);

    /// @notice Emitted when ownership is transferred.
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner);

    /// @notice Emitted when a two-step ownership transfer is initiated.
    event OwnershipTransferStarted(address indexed previousOwner, address indexed newOwner);

    /// @notice Contract administrator. Can rotate the agent and transfer ownership.
    address public owner;

    /// @notice Pending owner in a two-step transfer. Must call acceptOwnership() to confirm.
    address public pendingOwner;

    /// @notice The only address allowed to attest. Holds gas money only.
    address public agent;

    /// @notice Restricts a function to the configured agent.
    modifier onlyAgent() {
        require(msg.sender == agent, "OutcomeAttestor: not agent");
        _;
    }

    /// @notice Restricts a function to the owner.
    modifier onlyOwner() {
        require(msg.sender == owner, "OutcomeAttestor: not owner");
        _;
    }

    /// @param agent_ The initial agent address authorized to call `attestOutcome`.
    /// @dev   Deployer becomes the owner. `agent_` may equal the owner if desired.
    constructor(address agent_) {
        require(agent_ != address(0), "OutcomeAttestor: zero agent");
        owner = msg.sender;
        agent = agent_;
        emit OwnershipTransferred(address(0), msg.sender);
        emit AgentUpdated(address(0), agent_);
    }

    /// @notice Publish a signal outcome. Emits {OutcomeRecorded}; stores nothing.
    /// @param signalHash keccak256 of the canonical signal JSON (the same value the
    ///        signal was attested with on {SignalAttestor}).
    /// @param grade      Composite outcome score 0..100.
    /// @param reportHash keccak256 of the canonical off-chain report-card JSON.
    function attestOutcome(
        bytes32 signalHash,
        uint8 grade,
        bytes32 reportHash
    ) external onlyAgent {
        emit OutcomeRecorded(signalHash, grade, reportHash, uint64(block.timestamp));
    }

    /// @notice Rotate the agent address.
    /// @param a The new agent. Must be non-zero.
    function setAgent(address a) external onlyOwner {
        require(a != address(0), "OutcomeAttestor: zero agent");
        emit AgentUpdated(agent, a);
        agent = a;
    }

    /// @notice Begin a two-step ownership transfer. Sets `pendingOwner` to `o` and
    ///         emits {OwnershipTransferStarted}. The transfer is not final until the
    ///         new owner calls {acceptOwnership}. This prevents bricking all admin
    ///         powers by a mistyped-but-nonzero address.
    /// @param o The proposed new owner. Must be non-zero.
    function transferOwnership(address o) external onlyOwner {
        require(o != address(0), "OutcomeAttestor: zero owner");
        pendingOwner = o;
        emit OwnershipTransferStarted(owner, o);
    }

    /// @notice Complete a pending ownership transfer. Caller must be the current
    ///         `pendingOwner`; reverts otherwise. Clears `pendingOwner` after promoting.
    function acceptOwnership() external {
        require(msg.sender == pendingOwner, "OutcomeAttestor: not pending owner");
        address prev = owner;
        owner = pendingOwner;
        pendingOwner = address(0);
        emit OwnershipTransferred(prev, owner);
    }
}
