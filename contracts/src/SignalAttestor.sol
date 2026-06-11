// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

/// @title SignalAttestor
/// @notice Events-only on-chain attestation log for the OCR (OnChain Radar) AI agent.
///         Every anomaly signal the agent produces is published here as a tamper-proof,
///         timestamped event. The full agent track record is reconstructable purely from
///         logs; the contract stores no signal data, only access control.
/// @dev    Deliberately storage-free for signals: emitting an event costs almost no gas,
///         and the dashboard reads the entire history straight from the log topics.
///         Only `owner`, `pendingOwner`, and `agent` are persisted (for access control
///         and rotation).
///
///         Dedup policy: attest() emits unconditionally, with no on-chain
///         signalHash-seen guard. Duplicate suppression is the caller's responsibility:
///         the Go attestor never calls attest() for a signal that already has an
///         attest_tx in Postgres (guarded by the DB unique key on (pool, signal_type,
///         metric, bucket_ts)). Any consumer reading logs should dedup by signalHash
///         (earliest ts wins) when reconstructing the canonical track record.
///
///         Consumer filter invariant: SignalAttested does not include msg.sender, and
///         every field (signalHash, subject, signalType, score) is caller-supplied
///         input. The only thing tying a log to this agent is the emitting contract
///         address. Any eth_getLogs / FilterLogs consumer reconstructing the track
///         record must scope the query to this contract's address; matching on topic0
///         alone would let an unrelated contract emitting the same event mix its
///         attestations into the agent's history. Always set FilterQuery.Addresses =
///         [deployedSignalAttestorAddress] and never match by topic0 only.
contract SignalAttestor {
    /// @notice Emitted once per attested signal.
    /// @param signalHash keccak256 of the canonical signal JSON (off-chain dedupe / integrity key).
    /// @param subject    The address the signal is about (e.g. the pool address).
    /// @param signalType Signal category (indexed). 1 = flow_anomaly, 2 = smart_money, ... (extensible).
    /// @param score      z-score * 100, capped to fit uint16 (max 65535).
    /// @param ts         Block timestamp at attestation (seconds since epoch).
    event SignalAttested(
        bytes32 indexed signalHash,
        address indexed subject,
        uint8 indexed signalType,
        uint16 score,
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
        require(msg.sender == agent, "SignalAttestor: not agent");
        _;
    }

    /// @notice Restricts a function to the owner.
    modifier onlyOwner() {
        require(msg.sender == owner, "SignalAttestor: not owner");
        _;
    }

    /// @param agent_ The initial agent address authorized to call `attest`.
    /// @dev   Deployer becomes the owner. `agent_` may equal the owner if desired.
    constructor(address agent_) {
        require(agent_ != address(0), "SignalAttestor: zero agent");
        owner = msg.sender;
        agent = agent_;
        emit OwnershipTransferred(address(0), msg.sender);
        emit AgentUpdated(address(0), agent_);
    }

    /// @notice Publish a signal attestation. Emits {SignalAttested}; stores nothing.
    /// @param signalHash keccak256 of the canonical signal JSON.
    /// @param subject    The address the signal concerns (e.g. pool).
    /// @param signalType Signal category code.
    /// @param score      z-score * 100, capped to uint16.
    function attest(
        bytes32 signalHash,
        address subject,
        uint8 signalType,
        uint16 score
    ) external onlyAgent {
        emit SignalAttested(signalHash, subject, signalType, score, uint64(block.timestamp));
    }

    /// @notice Rotate the agent address.
    /// @param a The new agent. Must be non-zero.
    function setAgent(address a) external onlyOwner {
        require(a != address(0), "SignalAttestor: zero agent");
        emit AgentUpdated(agent, a);
        agent = a;
    }

    /// @notice Begin a two-step ownership transfer. Sets `pendingOwner` to `o` and
    ///         emits {OwnershipTransferStarted}. The transfer is not final until the
    ///         new owner calls {acceptOwnership}. This prevents bricking all admin
    ///         powers by a mistyped-but-nonzero address.
    /// @param o The proposed new owner. Must be non-zero.
    function transferOwnership(address o) external onlyOwner {
        require(o != address(0), "SignalAttestor: zero owner");
        pendingOwner = o;
        emit OwnershipTransferStarted(owner, o);
    }

    /// @notice Complete a pending ownership transfer. Caller must be the current
    ///         `pendingOwner`; reverts otherwise. Clears `pendingOwner` after promoting.
    function acceptOwnership() external {
        require(msg.sender == pendingOwner, "SignalAttestor: not pending owner");
        address prev = owner;
        owner = pendingOwner;
        pendingOwner = address(0);
        emit OwnershipTransferred(prev, owner);
    }
}
