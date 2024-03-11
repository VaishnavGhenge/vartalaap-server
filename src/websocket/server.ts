import { WebSocket, Server as WebSocketServer } from "ws";
import logger from "../Logger/logger";
import { IMessageData } from "../types/webcocketTypes";
import { MeetEvent } from "./config";
import { sendToPeer } from "./utils";
import { answer, createOffer, joinMeet, joinMeetLobby, leaveMeeting } from "./meets";

export const meets = new Map<string, { peersInLobby: Set<string>, peersInMeet: Set<string> }>();
export const sessionIdToSocketMap = new Map<string, WebSocket | null>();

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
            logger.info(`Message from ${sessionId} message: ${data.type}`);
            sessionIdToSocketMap.set(sessionId, ws);

            // Handle different types of messages
            switch (data.type) {
                case MeetEvent.JOIN_MEET_LOBBY:
                    joinMeetLobby(data.meetId, ws, sessionId);

                    break;
                case MeetEvent.JOIN_MEET:
                    logger.info("Join meet");
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
            logger.warn(`User got disconnected`);

            // Remove the session from the sessions map when the WebSocket connection is closed
            // sessions.delete(ws);
        });
    });
}
