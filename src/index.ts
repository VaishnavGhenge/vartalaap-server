import express from "express";
import http from "http";
import { Server as WebSocketServer, WebSocket } from "ws";
import logger from "./Logger/logger";
import expressWinston from "express-winston";
import { startWebSocketServer } from "./websocket/server";
import { startRestServer } from "./rest/server";

function startBackend() {
    const app = express();
    const server = http.createServer(app);
    const wss = new WebSocketServer({ server });

    const sessionMap = new Map<string, WebSocket | null>();

    // Apply the CORS middleware
    app.use(function (req, res, next) {
        res.header("Access-Control-Allow-Origin", "*");
        res.header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE");
        res.header("Access-Control-Allow-Headers", "Content-Type");
        next();
    });

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

    startRestServer(app, sessionMap);
    startWebSocketServer(wss);
}

startBackend();
