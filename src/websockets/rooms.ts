import { WebSocket } from "ws";
import { IMessageData } from "../types/webcocketTypes";
import { meets } from "./socketServer";
import { sendToMeetPeers } from "./utils";

export function joinMeet(meetId: string, ws: WebSocket) {
    if(!meets.has(meetId)) {
        meets.set(meetId, new Set());
    }

    meets.get(meetId)!.add(ws);

    const newUserJoinedMessage = {type: "peer-joined"}
    sendToMeetPeers(meetId, newUserJoinedMessage, ws, true);
};

export function createOffer(meetId: string, ws: WebSocket, offer: any) {
    const offerMessage = {type: "peer-offer-incoming", offer: offer};

    sendToMeetPeers(meetId, offerMessage, ws, true);
}

export function acceptOffer(meetId: string, ws: WebSocket, answer: any) {
    const answerMessage = {type: "peer-answer-incoming", answer: answer};

    sendToMeetPeers(meetId, answerMessage, ws, true);
}

export function leaveMeet(meetId: string, ws: WebSocket) {
    const meetPeersSet = meets.get(meetId);
    if(meetPeersSet) {
        meetPeersSet.delete(ws);
        
        if(meetPeersSet.size === 0) {
            meets.delete(meetId);
        }
    }

    const leaveMessage = {type: "peer-leave"};
    sendToMeetPeers(meetId, leaveMessage, ws);
};
