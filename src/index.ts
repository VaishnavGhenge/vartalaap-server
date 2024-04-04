import express from "express";
import http from "http";
import { Server as WebSocketServer, WebSocket } from "ws";
import logger from "./Logger/logger";
import expressWinston from "express-winston";
import { startWebSocketServer } from "./websocket/server";
import { startRestServer } from "./rest/server";
import cors from "cors";
import { allowedHosts } from "./config";

export const meets = new Map<string, { peersInLobby: Set<string>, peersInMeet: Set<string> }>();
export const sessionIdToMeetMap = new Map<string, string>();
export const sessionIdToSocketMap = new Map<string, WebSocket | null>();
export const socketToSessionMap = new Map<WebSocket, string>();

function startBackend() {
    const app = express();
    const server = http.createServer(app);
    const wss = new WebSocketServer({ server });

    const corsOptions = {
        origin: function(origin: string | undefined, callback: (error: Error | null, allow?: boolean) => void) {
            if(origin && allowedHosts.includes(origin)) {
                callback(null, true);
            } else {
                callback(new Error("Origin denied access by CORS"))
            }
        },
        optionsSuccessStatus: 204,
    }

    // Apply the CORS middleware
    app.use(cors(corsOptions));

    // Log requests and responses using Express-Winston middleware
    app.use(expressWinston.logger({
        winstonInstance: logger,
        meta: true, // Log metadata like request and response information
        msg: 'HTTP {{req.method}} {{res.statusCode}} {{req.url}} {{res.responseTime}}ms',
        colorize: true // Apply colors to the console output
    }));

    app.get("/", (req, res, next) => {
        res.send("Hello from vartalaap");
    })

    const PORT = Number(process.env.PORT) || 8080;
    server.listen(PORT, () => {
        logger.info(`Server running on port ${PORT}`);
    });

    startRestServer(app);
    startWebSocketServer(wss);
}

startBackend();
