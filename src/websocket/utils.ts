import { WebSocket } from "ws";
import { meets, sessionIdToSocketMap } from "./server";
import { MeetEvent } from "./config";

export function sendToMeetPeers(meetId: string, message: any, ws: WebSocket, exemptCurrentPeer = false) {
    try {
        const strigifiedMessage = JSON.stringify(message);
        const meetPeers = getMeetPeers(meetId);
        const meetPeerList = [...meetPeers.peersInMeet, ...meetPeers.peersInLobby];

        console.log(meetPeerList);

        const meetPeerSocketList = meetPeerList.map((peerSessionId) => sessionIdToSocketMap.get(peerSessionId));

        console.log(meetPeerSocketList);

        meetPeerSocketList.forEach((client) => {
            if(!client) {
                return;
            }

            if(client.readyState === WebSocket.OPEN) {
                if(ws !== client && exemptCurrentPeer) {
                    client.send(strigifiedMessage);
                    return;
                }

                client.send(strigifiedMessage);
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