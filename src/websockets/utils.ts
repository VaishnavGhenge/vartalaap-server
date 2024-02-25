import { WebSocket } from "ws";
import { meets } from "./socketServer";

export function sendToMeetPeers(meetId: string, message: any, ws: WebSocket, exemptCurrentPeer = false) {
    try {
        meets.get(meetId)?.forEach((client) => {
            const strigifiedMessage = JSON.stringify(message);
    
            if(client.readyState === WebSocket.OPEN) {
                if(exemptCurrentPeer && ws !== client) {
                    client.send(JSON.stringify(strigifiedMessage));
                }
            }
        });
    }
    catch(err) {
        console.error(`Error while sending message to meet peers \nError: ${err}`);
    }
}