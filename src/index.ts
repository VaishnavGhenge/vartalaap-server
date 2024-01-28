import express from "express";
import http from "http";
import { Server as WebSocketServer } from "ws";
import logger from "./Logger/logger";
import { v4 as uuidv4 } from "uuid";

function startServer() {
    const app = express();
    const server = http.createServer(app);
    const wss = new WebSocketServer({ server });

    app.use(express.static("public"));

    const PORT = process.env.PORT || 8080;
    server.listen(PORT, () => {
        logger.info(`Server running on port ${PORT}`);
    });

    // Map to store sessions
    const sessions = new Map();

    wss.on("connection", (ws) => {
        logger.info("New WebSocket connection");

        // Generate a new session ID and store it in the sessions map
        const sessionId = uuidv4();
        sessions.set(ws, { id: sessionId });

        ws.on("message", (message: string) => {
            // Parse the message as JSON
            let data;
            try {
                data = JSON.parse(message);
            } catch (err) {
                logger.error(`Could not parse message: ${message}`);
                return;
            }

            // Handle different types of messages
            switch (data.type) {
                case "join":
                    // Handle a join message
                    logger.info(
                        `Session ${sessionId} joined with data: ${data}`,
                    );
                    break;
                case "leave":
                    // Handle a leave message
                    logger.info(`Session ${sessionId} left with data: ${data}`);
                    break;
                default:
                    logger.error(`Unknown message type: ${data.type}`);
            }

            // Broadcast any message received to all clients
            wss.clients.forEach((client) => {
                if (client !== ws && client.readyState === WebSocket.OPEN) {
                    client.send(message);
                }
            });
        });

        ws.on("close", () => {
            // Remove the session from the sessions map when the WebSocket connection is closed
            sessions.delete(ws);
        });
    });
}

startServer();
