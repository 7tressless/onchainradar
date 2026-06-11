// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Test} from "forge-std/Test.sol";
import {SignalAttestor} from "../src/SignalAttestor.sol";

contract SignalAttestorTest is Test {
    SignalAttestor internal attestor;

    address internal owner = address(this);
    address internal agent = address(0xA9E47);
    address internal stranger = address(0xBEEF);

    // Mirror contract events for expectEmit matching.
    // signalType is now indexed (3rd topic), so checkTopic3=true in expectEmit.
    event SignalAttested(
        bytes32 indexed signalHash,
        address indexed subject,
        uint8 indexed signalType,
        uint16 score,
        uint64 ts
    );
    event AgentUpdated(address indexed previousAgent, address indexed newAgent);
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner);
    event OwnershipTransferStarted(address indexed previousOwner, address indexed newOwner);

    function setUp() public {
        attestor = new SignalAttestor(agent);
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

        new SignalAttestor(agent);
    }

    /// @dev Constructor must revert when the agent argument is the zero address.
    function test_Constructor_RevertsOnZeroAgent() public {
        vm.expectRevert(bytes("SignalAttestor: zero agent"));
        new SignalAttestor(address(0));
    }

    // attest()

    /// @dev Happy path: verify every field of the emitted event including ts == block.timestamp.
    ///      signalType is now indexed (3rd topic); checkTopic3=true.
    function test_Attest_EmitsCorrectArgs() public {
        bytes32 signalHash = keccak256("canonical-signal-json");
        address subject = address(0x9001); // stand-in pool address
        uint8 signalType = 1; // flow_anomaly
        uint16 score = 412; // z 4.12 * 100

        // Pin the timestamp so the emitted ts is deterministic.
        uint64 nowTs = 1_750_000_000;
        vm.warp(nowTs);

        // checkTopic1=true, checkTopic2=true, checkTopic3=true, checkData=true
        vm.expectEmit(true, true, true, true, address(attestor));
        emit SignalAttested(signalHash, subject, signalType, score, nowTs);

        vm.prank(agent);
        attestor.attest(signalHash, subject, signalType, score);
    }

    /// @dev ts field in the event must equal block.timestamp at the moment of the call.
    function test_Attest_TsEqualsBlockTimestamp() public {
        uint64 warpedTs = 1_800_000_000;
        vm.warp(warpedTs);

        bytes32 h = keccak256("ts-check");
        address sub = address(0x42);

        vm.expectEmit(true, true, true, true, address(attestor));
        emit SignalAttested(h, sub, 1, 999, warpedTs);

        vm.prank(agent);
        attestor.attest(h, sub, 1, 999);
    }

    /// @dev A random address that is neither owner nor agent must be rejected.
    function test_Attest_RevertsForNonAgent() public {
        vm.expectRevert(bytes("SignalAttestor: not agent"));
        vm.prank(stranger);
        attestor.attest(keccak256("x"), address(0x1234), 1, 100);
    }

    /// @dev The owner is not automatically the agent; attest must still gate on agent.
    function test_Attest_RevertsForOwnerWhoIsNotAgent() public {
        vm.expectRevert(bytes("SignalAttestor: not agent"));
        attestor.attest(keccak256("x"), address(0x1234), 1, 100);
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
        attestor.attest(keccak256("y"), address(0x1), 1, 50);

        // Old agent is now rejected.
        vm.expectRevert(bytes("SignalAttestor: not agent"));
        vm.prank(agent);
        attestor.attest(keccak256("y"), address(0x1), 1, 50);
    }

    function test_SetAgent_RevertsForNonOwner() public {
        vm.expectRevert(bytes("SignalAttestor: not owner"));
        vm.prank(stranger);
        attestor.setAgent(address(0xC0DE));
    }

    function test_SetAgent_RevertsOnZeroAddress() public {
        vm.expectRevert(bytes("SignalAttestor: zero agent"));
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
        vm.expectRevert(bytes("SignalAttestor: not owner"));
        vm.prank(stranger);
        attestor.transferOwnership(stranger);
    }

    function test_TransferOwnership_RevertsOnZeroAddress() public {
        vm.expectRevert(bytes("SignalAttestor: zero owner"));
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
        vm.expectRevert(bytes("SignalAttestor: not owner"));
        attestor.setAgent(address(0xC0DE));

        // New owner can exercise owner powers.
        vm.prank(stranger);
        attestor.setAgent(address(0xDEAD));
        assertEq(attestor.agent(), address(0xDEAD));
    }

    /// @dev acceptOwnership must revert for anyone who is not the pending owner.
    function test_AcceptOwnership_RevertsForNonPendingOwner() public {
        attestor.transferOwnership(stranger);

        vm.expectRevert(bytes("SignalAttestor: not pending owner"));
        // Caller is address(this) == owner, not stranger == pendingOwner.
        attestor.acceptOwnership();
    }

    /// @dev acceptOwnership must revert when no transfer has been initiated.
    function test_AcceptOwnership_RevertsWhenNoPendingTransfer() public {
        vm.expectRevert(bytes("SignalAttestor: not pending owner"));
        vm.prank(stranger);
        attestor.acceptOwnership();
    }

    // Fuzz: attest() accepts any valid input without reverting

    /// @dev The agent must be able to attest any combination of inputs.
    ///      Subject is constrained to non-zero to match realistic pool addresses,
    ///      though the contract itself imposes no such restriction; the test uses
    ///      bound() to ensure subject != address(0) so the fuzz run is meaningful.
    function testFuzz_Attest_AcceptsArbitraryInputs(
        bytes32 signalHash,
        address subject,
        uint8 signalType,
        uint16 score
    ) public {
        // Ensure subject is a realistic pool address (non-zero).
        vm.assume(subject != address(0));

        uint64 warpedTs = 1_750_000_000;
        vm.warp(warpedTs);

        vm.expectEmit(true, true, true, true, address(attestor));
        emit SignalAttested(signalHash, subject, signalType, score, warpedTs);

        vm.prank(agent);
        attestor.attest(signalHash, subject, signalType, score);
    }

    /// @dev Any caller that is not the agent must be rejected, regardless of inputs.
    function testFuzz_Attest_RevertsForNonAgent(
        address caller,
        bytes32 signalHash,
        address subject,
        uint8 signalType,
        uint16 score
    ) public {
        vm.assume(caller != agent);

        vm.expectRevert(bytes("SignalAttestor: not agent"));
        vm.prank(caller);
        attestor.attest(signalHash, subject, signalType, score);
    }
}
