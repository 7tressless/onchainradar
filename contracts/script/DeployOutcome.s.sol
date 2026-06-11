// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Script, console} from "forge-std/Script.sol";
import {OutcomeAttestor} from "../src/OutcomeAttestor.sol";

/// @title DeployOutcome
/// @notice Deploys {OutcomeAttestor}, passing the agent address from the AGENT_ADDRESS env var.
/// @dev    Sidecar deploy, independent of {SignalAttestor}; deploying this never touches the
///         signal attestor. The agent may be the same OCR agent wallet used for SignalAttestor
///         (it only needs gas to call attestOutcome). Broadcasting key is supplied by
///         `forge script` via --private-key / --ledger / keystore; this script never reads
///         private key material itself. Only AGENT_ADDRESS is read here.
contract DeployOutcome is Script {
    function run() external returns (OutcomeAttestor attestor) {
        address agent = vm.envAddress("AGENT_ADDRESS");
        require(agent != address(0), "DeployOutcome: AGENT_ADDRESS is zero");

        vm.startBroadcast();
        attestor = new OutcomeAttestor(agent);
        vm.stopBroadcast();

        console.log("OutcomeAttestor deployed at:", address(attestor));
        console.log("owner:", attestor.owner());
        console.log("agent:", attestor.agent());
    }
}
