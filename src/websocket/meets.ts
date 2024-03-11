import { WebSocket } from "ws";
import { doMeetExists, getMeetPeers, initiateMeet, sendToMeetPeers, sendToPeer } from "./utils";
import { MeetEvent } from "./config";
import { meets, sessionIdToSocketMap } from "./server";
import logger from "../Logger/logger";

export function joinMeetLobby(meetId: string, ws: WebSocket, sessionId: string) {
    if (!doMeetExists(meetId, ws)) {
        return;
    }

    const { peersInMeet, peersInLobby } = getMeetPeers(meetId);

    if (!peersInLobby.has(sessionId)) {
        peersInLobby.add(sessionId);
        meets.set(meetId, { peersInLobby, peersInMeet });
    }

    const newUserJoinedMessage = {
        type: MeetEvent.PEER_JOINED,
        sessionIdList: Array.from(peersInMeet),
    };
    sendToPeer(ws, newUserJoinedMessage);

    logger.info(`${sessionId} joined meet ${meetId} lobby`);
}

export function joinMeet(meetId: string, ws: WebSocket, sessionId: string) {
    if (!doMeetExists(meetId, ws)) {
        return;
    }

    const { peersInMeet, peersInLobby } = getMeetPeers(meetId);

    if (peersInLobby.has(sessionId)) {
        peersInLobby.delete(sessionId);
    }

    if (!peersInMeet.has(sessionId)) {
        if (peersInMeet.size === 2) {
            sendToPeer(ws, { type: MeetEvent.BAD_REQUEST, message: "Meet is full" });
            return;
        }

        peersInMeet.add(sessionId);
    }

    meets.set(meetId, { peersInMeet, peersInLobby });

    const newUserJoinedMessage = {
        type: MeetEvent.PEER_JOINED,
        sessionIdList: Array.from(peersInMeet),
    };
    sendToMeetPeers(meetId, newUserJoinedMessage, ws);

    if (peersInMeet.size === 2) {
        initiateMeet(meetId, peersInMeet);
    }

    logger.info(`${sessionId} joined meet ${meetId}`);
}

export function leaveMeeting(meetId: string, ws: WebSocket, sessionId: string) {
    if (!doMeetExists(meetId, ws)) {
        return;
    }

    // Remove peer from meet
    const { peersInMeet, peersInLobby } = getMeetPeers(meetId);
    if (peersInMeet.has(sessionId)) {
        peersInMeet.delete(sessionId);
    }

    if (peersInLobby.has(sessionId)) {
        peersInLobby.delete(sessionId);
    }
    meets.set(meetId, { peersInMeet, peersInLobby });

    sessionIdToSocketMap.delete(sessionId);
    sendToMeetPeers(meetId, { type: MeetEvent.PEER_LEFT, sessionId: sessionId }, ws, true);

    logger.info(`${sessionId} left meeting ${meetId}`);
}

export function createOffer(meetId: string, ws: WebSocket, sessionId: string, offer: RTCDataChannelEventInit) {
    if(!doMeetExists(meetId, ws)) {
        return;
    }

    const meetPeers = meets.get(meetId) || null;
    if(meetPeers === null) {
        logger.error("Logical error: meet peers empty");
        return;
    }

    const peerToOffer = Array.from(meetPeers.peersInMeet).find((peerSessionId) => peerSessionId !== sessionId) || null;

    if(peerToOffer === null) {
        logger.error("Logical Error: Peer not found to send peer");
        return;
    }

    const peerToOfferSocket = sessionIdToSocketMap.get(peerToOffer) || null;

    if(peerToOfferSocket === null) {
        logger.error("Logical Error: Socket not found for peer to offer");
        return;
    }

    sendToPeer(peerToOfferSocket, {type: MeetEvent.OFFER, offer: offer});

    logger.info(`Offer sent to ${peerToOffer} on meet ${meetId}`);
}

export function answer(meetId: string, ws: WebSocket, sessionId: string, answer: any) {
    const meetPeers = meets.get(meetId) || null;
    if(meetPeers === null) {
        logger.error("Logical error: meet peers empty");
        return;
    }

    const peerToAnswer = Array.from(meetPeers.peersInMeet).find((peerSessionId) => peerSessionId !== sessionId) || null;

    if(peerToAnswer === null) {
        logger.error("Logical Error: Peer not found to send peer");
        return;
    }

    const peerToAnswerSocket = sessionIdToSocketMap.get(peerToAnswer) || null;

    if(peerToAnswerSocket === null) {
        logger.error("Logical Error: Socket not found for peer to answer");
        return;
    }

    const answerMessage = { type: MeetEvent.ANSWER, answer: answer };
    sendToPeer(peerToAnswerSocket, answerMessage);

    logger.info(`Answer sent to ${peerToAnswer} on meet ${meetId}`);
}