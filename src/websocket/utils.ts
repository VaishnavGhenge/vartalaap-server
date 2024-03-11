import { WebSocket } from "ws";
import { meets, sessionIdToSocketMap } from "./server";
import { MeetEvent } from "./config";
import logger from "../Logger/logger";

export function sendToMeetPeers(meetId: string, message: any, ws: WebSocket, exemptCurrentPeer = false) {
    try {
        const strigifiedMessage = JSON.stringify(message);
        const meetPeers = getMeetPeers(meetId);
        const meetPeerList = [...meetPeers.peersInMeet, ...meetPeers.peersInLobby];

        console.log("meet list: ", meetPeerList);

        const meetPeerSocketList = meetPeerList.map((peerSessionId) => sessionIdToSocketMap.get(peerSessionId));

        meetPeerSocketList.forEach((client) => {
            if(!client) {
                return;
            }

            if(client.readyState === WebSocket.OPEN) {
                if(ws === client && exemptCurrentPeer) {
                    client.send(strigifiedMessage);
                    return;
                } else {
                    client.send(strigifiedMessage);
                }
            }
        });
    }
    catch(err) {
        console.error(`Error while sending message to meet peers \nError: ${err}`);
    }
}

export function sendToPeer(ws: WebSocket, message: any) {
    try {
        ws.send(JSON.stringify(message));
    }
    catch(err) {
        console.error(`Error while sending message to meet peers \nError: ${err}`);
    }
}

export function doMeetExists(meetId: string, ws: WebSocket) {
    if(!meets.has(meetId)) {
        sendToPeer(ws, {type: MeetEvent.NOT_FOUND, message: "Meet not found"});
        return false;
    }

    return true;
}

export function getMeetPeers(meetId: string): {peersInLobby: Set<string>, peersInMeet: Set<string>} {
    let meetPeers = meets.get(meetId);

    if(!meetPeers) {
        meetPeers = {peersInLobby: new Set(), peersInMeet: new Set()}
        meets.set(meetId, meetPeers);
    }

    return meetPeers;
}

export function initiateMeet(meetId: string, peerSessionsInMeet: Set<string>) {
    if(peerSessionsInMeet.size === 2) {
        logger.warn("initiated meet");

        const firstPeerSessionId = Array.from(peerSessionsInMeet)[0];
        const client: WebSocket | null = sessionIdToSocketMap.get(firstPeerSessionId) || null;

        if(!client) {
            logger.error(`Logical Error: Did not found websocket of meet connected peer`);
            return;
        }

        sendToPeer(client, {type: MeetEvent.INITIATE_MEET_REQUEST});
    }
}