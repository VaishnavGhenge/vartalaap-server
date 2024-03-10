import { WebSocket } from "ws";
import { doMeetExists, getMeetPeers, sendToMeetPeers, sendToPeer } from "./utils";
import { MeetEvent } from "./config";
import { meets } from "./server";
import logger from "../Logger/logger";

export function joinMeetLobby(meetId: string, ws: WebSocket, sessionId: string) {
    if(!doMeetExists(meetId, ws)) {
        return;
    }

    const {peersInMeet, peersInLobby} = getMeetPeers(meetId);

    if(!peersInLobby.has(sessionId)) {
        peersInLobby.add(sessionId);
        meets.set(meetId, {peersInLobby, peersInMeet});
    }

    const newUserJoinedMessage = {
        type: MeetEvent.PEER_JOINED_LOBBY,
        sessionIdList: Array.from(peersInMeet),
    };
    sendToPeer(ws, newUserJoinedMessage);

    logger.info(`${sessionId} joined meet ${meetId} lobby`);
}

export function joinMeet(meetId: string, ws: WebSocket, sessionId: string) {
    if (!doMeetExists(meetId, ws)) {
        return;
    }

    const {peersInMeet, peersInLobby} = getMeetPeers(meetId);

    if (!peersInMeet.has(sessionId)) {
        peersInMeet.add(sessionId);
    }

    if(peersInLobby.has(sessionId)) {
        peersInLobby.delete(sessionId);
    }

    meets.set(meetId, {peersInMeet, peersInLobby});

    const newUserJoinedMessage = {
        type: MeetEvent.PEER_JOINED_LOBBY,
        sessionIdList: Array.from(peersInMeet),
    };
    sendToMeetPeers(meetId, newUserJoinedMessage, ws);

    logger.info(`${sessionId} joined meet ${meetId}`);
}

export function createOffer(meetId: string, ws: WebSocket, offer: any) {
    const offerMessage = { type: "peer-offer-incoming", offer: offer };

    sendToMeetPeers(meetId, offerMessage, ws, true);
}

export function acceptOffer(meetId: string, ws: WebSocket, answer: any) {
    const answerMessage = { type: "peer-answer-incoming", answer: answer };

    sendToMeetPeers(meetId, answerMessage, ws, true);
}