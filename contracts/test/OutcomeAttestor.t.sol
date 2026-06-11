// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test} from "forge-std/Test.sol";
import {OutcomeAttestor} from "../src/OutcomeAttestor.sol";

contract OutcomeAttestorTest is Test {
    OutcomeAttestor internal attestor;

    address internal owner = address(this);
    address internal agent = address(0xA9E47);
    address internal stranger = address(0xBEEF);

    // Mirror contract events for expectEmit matching.
    // In OutcomeRecorded only signalHash is indexed (1st topic); grade/reportHash/ts
    // are in the data, so checkTopic1=true, checkTopic2/3=false, checkData=true.
    event OutcomeRecorded(
        bytes32 indexed signalHash,
        uint8 grade,
        bytes32 reportHash,
        uint64 ts
    );
    event AgentUpdated(address indexed previousAgent, address indexed newAgent);
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner);
    event OwnershipTransferStarted(address indexed previousOwner, address indexed newOwner);

    function setUp() public {
        attestor = new OutcomeAttestor(agent);
    }

    // Constructor

    function test_ConstructorSetsOwnerAndAgent() public view {
        assertEq(attestor.owner(), owner, "owner should be deployer");
        assertEq(attestor.agent(), agent, "agent should be constructor arg");
        assertEq(attestor.pendingOwner(), address(0), "pendingOwner should be zero after construction");
    }

    /// @dev Constructor must emit OwnershipTransferred(address(0), deployer).
    function test_Constructor_EmitsOwnershipTransferred() public {
        vm.expectEmit(true, true, false, false);
        emit OwnershipTransferred(address(0), owner);

        vm.expectEmit(true, true, false, false);
        emit AgentUpdated(address(0), agent);

        new OutcomeAttestor(agent);
    }

    /// @dev Constructor must revert when the agent argument is the zero address.
    function test_Constructor_RevertsOnZeroAgent() public {
        vm.expectRevert(bytes("OutcomeAttestor: zero agent"));
        new OutcomeAttestor(address(0));
    }

    // attestOutcome()

    /// @dev Happy path: verify every field of the emitted event including ts == block.timestamp.
    ///      Only signalHash is indexed; checkData=true asserts grade/reportHash/ts.
    function test_AttestOutcome_EmitsCorrectArgs() public {
        bytes32 signalHash = keccak256("canonical-signal-json");
        uint8 grade = 87; // composite outcome score
        bytes32 reportHash = keccak256("canonical-report-card-json");

        // Pin the timestamp so the emitted ts is deterministic.
        uint64 nowTs = 1_750_000_000;
        vm.warp(nowTs);

        // checkTopic1=true, checkTopic2=false, checkTopic3=false, checkData=true
        vm.expectEmit(true, false, false, true, address(attestor));
        emit OutcomeRecorded(signalHash, grade, reportHash, nowTs);

        vm.prank(agent);
        attestor.attestOutcome(signalHash, grade, reportHash);
    }

    /// @dev ts field in the event must equal block.timestamp at the moment of the call.
    function test_AttestOutcome_TsEqualsBlockTimestamp() public {
        uint64 warpedTs = 1_800_000_000;
        vm.warp(warpedTs);

        bytes32 h = keccak256("ts-check");
        bytes32 r = keccak256("report");

        vm.expectEmit(true, false, false, true, address(attestor));
        emit OutcomeRecorded(h, 100, r, warpedTs);

        vm.prank(agent);
        attestor.attestOutcome(h, 100, r);
    }

    /// @dev A random address that is neither owner nor agent must be rejected.
    function test_AttestOutcome_RevertsForNonAgent() public {
        vm.expectRevert(bytes("OutcomeAttestor: not agent"));
        vm.prank(stranger);
        attestor.attestOutcome(keccak256("x"), 50, keccak256("r"));
    }

    /// @dev The owner is not automatically the agent; attestOutcome must still gate on agent.
    function test_AttestOutcome_RevertsForOwnerWhoIsNotAgent() public {
        vm.expectRevert(bytes("OutcomeAttestor: not agent"));
        attestor.attestOutcome(keccak256("x"), 50, keccak256("r"));
    }

    // setAgent()

    /// @dev Owner can rotate the agent; the AgentUpdated event carries both addresses;
    ///      the new agent can attest; the old agent is rejected.
    function test_SetAgent_WorksForOwner() public {
        address newAgent = address(0xC0DE);

        // Verify full event: both indexed topics must match.
        vm.expectEmit(true, true, false, false, address(attestor));
        emit AgentUpdated(agent, newAgent);

        attestor.setAgent(newAgent);
        assertEq(attestor.agent(), newAgent, "agent should be updated");

        // New agent can attest.
        vm.prank(newAgent);
        attestor.attestOutcome(keccak256("y"), 50, keccak256("r"));

        // Old agent is now rejected.
        vm.expectRevert(bytes("OutcomeAttestor: not agent"));
        vm.prank(agent);
        attestor.attestOutcome(keccak256("y"), 50, keccak256("r"));
    }

    function test_SetAgent_RevertsForNonOwner() public {
        vm.expectRevert(bytes("OutcomeAttestor: not owner"));
        vm.prank(stranger);
        attestor.setAgent(address(0xC0DE));
    }

    function test_SetAgent_RevertsOnZeroAddress() public {
        vm.expectRevert(bytes("OutcomeAttestor: zero agent"));
        attestor.setAgent(address(0));
    }

    // transferOwnership is two-step: it initiates but does not commit immediately

    /// @dev transferOwnership sets pendingOwner and emits OwnershipTransferStarted.
    ///      It does not yet change the current owner.
    function test_TransferOwnership_SetsPendingOwner() public {
        vm.expectEmit(true, true, false, false, address(attestor));
        emit OwnershipTransferStarted(owner, stranger);

        attestor.transferOwnership(stranger);

        assertEq(attestor.owner(), owner, "owner must not change until accepted");
        assertEq(attestor.pendingOwner(), stranger, "pendingOwner should be set");
    }

    /// @dev Old owner retains full powers (setAgent) until acceptOwnership is called.
    function test_TransferOwnership_OldOwnerRetainsPowersUntilAccept() public {
        attestor.transferOwnership(stranger);
        // Old owner can still rotate the agent.
        attestor.setAgent(address(0xC0DE));
        assertEq(attestor.agent(), address(0xC0DE));
    }

    function test_TransferOwnership_RevertsForNonOwner() public {
        vm.expectRevert(bytes("OutcomeAttestor: not owner"));
        vm.prank(stranger);
        attestor.transferOwnership(stranger);
    }

    function test_TransferOwnership_RevertsOnZeroAddress() public {
        vm.expectRevert(bytes("OutcomeAttestor: zero owner"));
        attestor.transferOwnership(address(0));
    }

    // acceptOwnership completes the two-step transfer

    /// @dev Full two-step round-trip: initiate -> accept -> old owner loses powers.
    function test_AcceptOwnership_CompletesTransfer() public {
        attestor.transferOwnership(stranger);

        vm.expectEmit(true, true, false, false, address(attestor));
        emit OwnershipTransferred(owner, stranger);

        vm.prank(stranger);
        attestor.acceptOwnership();

        assertEq(attestor.owner(), stranger, "owner should now be stranger");
        assertEq(attestor.pendingOwner(), address(0), "pendingOwner should be cleared");

        // Old owner loses owner powers.
        vm.expectRevert(bytes("OutcomeAttestor: not owner"));
        attestor.setAgent(address(0xC0DE));

        // New owner can exercise owner powers.
        vm.prank(stranger);
        attestor.setAgent(address(0xDEAD));
        assertEq(attestor.agent(), address(0xDEAD));
    }

    /// @dev acceptOwnership must revert for anyone who is not the pending owner.
    function test_AcceptOwnership_RevertsForNonPendingOwner() public {
        attestor.transferOwnership(stranger);

        vm.expectRevert(bytes("OutcomeAttestor: not pending owner"));
        // Caller is address(this) == owner, not stranger == pendingOwner.
        attestor.acceptOwnership();
    }

    /// @dev acceptOwnership must revert when no transfer has been initiated.
    function test_AcceptOwnership_RevertsWhenNoPendingTransfer() public {
        vm.expectRevert(bytes("OutcomeAttestor: not pending owner"));
        vm.prank(stranger);
        attestor.acceptOwnership();
    }

    // Fuzz: attestOutcome() accepts any valid input without reverting

    /// @dev The agent must be able to attest any combination of inputs. The contract
    ///      imposes no on-chain range check on grade (the caller keeps it in [0,100]),
    ///      so the fuzz run exercises the full uint8 range.
    function testFuzz_AttestOutcome_AcceptsArbitraryInputs(
        bytes32 signalHash,
        uint8 grade,
        bytes32 reportHash
    ) public {
        uint64 warpedTs = 1_750_000_000;
        vm.warp(warpedTs);

        vm.expectEmit(true, false, false, true, address(attestor));
        emit OutcomeRecorded(signalHash, grade, reportHash, warpedTs);

        vm.prank(agent);
        attestor.attestOutcome(signalHash, grade, reportHash);
    }

    /// @dev Any caller that is not the agent must be rejected, regardless of inputs.
    function testFuzz_AttestOutcome_RevertsForNonAgent(
        address caller,
        bytes32 signalHash,
        uint8 grade,
        bytes32 reportHash
    ) public {
        vm.assume(caller != agent);

        vm.expectRevert(bytes("OutcomeAttestor: not agent"));
        vm.prank(caller);
        attestor.attestOutcome(signalHash, grade, reportHash);
    }
}
