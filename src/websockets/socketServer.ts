import { WebSocket, Server as WebSocketServer } from "ws";
import logger from "../Logger/logger";
import { v4 as uuidv4 } from "uuid";
import { acceptOffer, createOffer, joinMeet } from "./rooms";
import { IMessageData } from "../types/webcocketTypes";

export const meets = new Map<string, Set<WebSocket>>();

export function startWebSockerServer(wss: WebSocketServer) {
    // Map to store sessions
    const sessions = new Map();

    wss.on("connection", (ws: WebSocket) => {
        logger.info("New WebSocket connection");

        // Generate a new session ID and store it in the sessions map
        const sessionId = uuidv4();
        sessions.set(ws, { id: sessionId });

        ws.on("message", (message: string) => {
            // Parse the message as JSON
            let data: IMessageData;
            try {
                data = JSON.parse(message);
            } catch (err) {
                logger.error(`Could not parse message: ${message} from user: ${sessions.get(ws).id}`);
                return;
            }

            // Handle different types of messages
            switch (data.type) {
                case "join-meet":
                    // Handle a join message
                    joinMeet(data.meetId, ws);
                    logger.info(
                        `Session ${sessionId} joined with data: ${JSON.stringify(
                            data,
                        )}`,
                    );
                    break;
                case "offer":
                    createOffer(data.meetId, ws, data.offer);
                    logger.info(`Session ${sessionId} created offer to meet ${data.meetId}`);

                    break;
                case "answer":
                    acceptOffer(data.meetId, ws, data.answer);
                    logger.info(`Session ${sessionId} created answer to offer on meet ${data.meetId}`);

                    break;
                case "leave-meet":
                    // Handle a leave message
                    logger.info(
                        `Session ${sessionId} left with data: ${JSON.stringify(
                            data,
                        )}`,
                    );
                    break;
                default:
                    logger.error(`Unknown message type: ${data.type}`);
            }

            // Broadcast any message received to all clients
            // wss.clients.forEach((client) => {
            //     if (client !== ws && client.readyState === WebSocket.OPEN) {
            //         client.send(message);
            //     }
            // });
        });

        ws.on("close", () => {
            // Remove the session from the sessions map when the WebSocket connection is closed
            sessions.delete(ws);
        });
    });
}
