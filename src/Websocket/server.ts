import { WebSocket, Server as WebSocketServer } from "ws";
import logger from "../Logger/logger";
import { IMessageData } from "../Types/webcocketTypes";
import { MeetEvent } from "./config";
import { getMeetPeers, sendToMeetPeers, sendToPeer } from "./utils";
import { answer, createOffer, joinMeet, joinMeetLobby, leaveMeeting } from "./meets";
import { sessionIdToMeetMap, sessionIdToSocketMap, socketToSessionMap } from "../index";

export function startWebSocketServer(wss: WebSocketServer) {
    wss.on("connection", (ws: WebSocket) => {
        logger.warn("New user connected");

        ws.on("message", (message: string) => {
            // Parse the message as JSON
            let data: IMessageData;
            try {
                data = JSON.parse(message);
            } catch (err) {
                logger.error(`Could not parse message: ${message}`);
                return;
            }

            if (!data.sessionId) {
                sendToPeer(ws, { type: MeetEvent.BAD_REQUEST, message: "Missing sessionId in message data." });
                return;
            }

            const sessionId = data.sessionId;

            sessionIdToSocketMap.set(sessionId, ws);
            socketToSessionMap.set(ws, sessionId); // when user disconnects remove session.

            // Handle different types of messages
            switch (data.type) {
                case MeetEvent.JOIN_MEET_LOBBY:
                    joinMeetLobby(data.meetId, ws, sessionId);

                    break;
                case MeetEvent.JOIN_MEET:
                    joinMeet(data.meetId, ws, sessionId);

                    break;
                case MeetEvent.CREATE_OFFER:
                    createOffer(data.meetId, ws, sessionId, data.offer);

                    break;
                case MeetEvent.CREATE_ANSWER:
                    answer(data.meetId, ws, sessionId, data.answer);

                    break;
                case MeetEvent.LEAVE_MEET:
                    leaveMeeting(data.meetId, ws, sessionId);

                    break;
                default:
                    logger.error(`Unknown message type: ${data.type}`);
            }
        });

        ws.on("close", () => {
            // Remove the session from the sessions map when the WebSocket connection is closed
            if(socketToSessionMap.has(ws)) {
                const sessionId = socketToSessionMap.get(ws)!;

                if(sessionIdToMeetMap.has(sessionId)) {
                    const meetId = sessionIdToMeetMap.get(sessionId)!;

                    const meetPeers = getMeetPeers(meetId);
                    meetPeers.peersInLobby.delete(sessionId);
                    meetPeers.peersInMeet.delete(sessionId);

                    sessionIdToSocketMap.delete(sessionId);
                    socketToSessionMap.delete(ws);

                    socketToSessionMap.delete(ws);

                    // Notify all connected peers, leaving of a current connection
                    const leaveMessage = {
                        type: MeetEvent.PEER_LEFT,
                        sessionId: sessionId,
                    };
                    sendToMeetPeers(meetId, leaveMessage, ws, true);

                    logger.info(`user - ${sessionId} disconnected and hence left meet - ${meetId}`);
                }
            } else {
                logger.warn(`Unknown user got disconnected`);
            }
        });
    });
}
