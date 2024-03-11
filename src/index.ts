import express from "express";
import http from "http";
import { Server as WebSocketServer, WebSocket } from "ws";
import logger from "./Logger/logger";
import expressWinston from "express-winston";
import { startWebSocketServer } from "./websocket/server";
import { startRestServer } from "./rest/server";
import cors from "cors";

function startBackend() {
    const app = express();
    const server = http.createServer(app);
    const wss = new WebSocketServer({ server });

    // Apply the CORS middleware
    // Configure CORS to allow only specific origins
    const corsOptions = {
        origin: ['https://vartalaap-client.vercel.app', 'http://localhost:3000'],
        methods: 'GET,HEAD,PUT,PATCH,POST,DELETE',
        optionsSuccessStatus: 204,
    };

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
