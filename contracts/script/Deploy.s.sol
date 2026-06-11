// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {Script, console} from "forge-std/Script.sol";
import {SignalAttestor} from "../src/SignalAttestor.sol";

/// @title Deploy
/// @notice Deploys {SignalAttestor}, passing the agent address from the AGENT_ADDRESS env var.
/// @dev    Broadcasting key is supplied by `forge script` via --private-key / --ledger / keystore;
///         this script never reads private key material itself. Only AGENT_ADDRESS is read here.
contract Deploy is Script {
    function run() external returns (SignalAttestor attestor) {
        address agent = vm.envAddress("AGENT_ADDRESS");
        require(agent != address(0), "Deploy: AGENT_ADDRESS is zero");

        vm.startBroadcast();
        attestor = new SignalAttestor(agent);
        vm.stopBroadcast();

        console.log("SignalAttestor deployed at:", address(attestor));
        console.log("owner:", attestor.owner());
        console.log("agent:", attestor.agent());
    }
}
