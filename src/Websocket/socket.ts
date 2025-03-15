import { WebSocket, WebSocketServer } from "ws";
import http from "http";
import { randomUUID } from "node:crypto";

import { logger } from "../Logger/logger";
import { TokenUserPayload } from "../Rest/library/utils";
import { Meet } from "./meet";
import { MessageSchemas, ParsedMessageData } from "./validation/Message";
import { GeneralMessage, MeetEvent } from "./config";
import { ZodError } from "zod";
import jwt from "jsonwebtoken";
import { config } from "../Config/config";

export interface Session {
    id: string;
}

export interface UnAuthenticatedSession extends Session {
    name: string;
}

export interface AuthenticatedSession extends Session {
    user: TokenUserPayload;
}

export class Socket {
    static wss: WebSocketServer | null = null;
    static meetManager: Meet;

    constructor(server: http.Server) {
        Socket.wss = this._createOrGetWebSocketServer(server);
        Socket.meetManager = new Meet();

        if (Socket.wss) {
            Socket.wss.on("connection", this._handleConnection);
        }
    }

    private _createOrGetWebSocketServer(server: http.Server) {
        if (Socket.wss) {
            return Socket.wss;
        }

        try {
            return new WebSocketServer({ server });
        } catch (error) {
            logger.error("WebSocket Server Error", error);
            return null;
        }
    }

    private _createUserSession(): Session {
        return { id: randomUUID() };
    }

    private _handleConnection(ws: WebSocket) {
        const session = this._createUserSession();

        ws.on("message", (rawMessage: string) => this._handleMessage(session, ws, rawMessage));
        ws.on("close", () => this._handleDisconnection(session));
    }

    private _handleMessage(session: Session, ws: WebSocket, message: string) {
        try {
            const rawData: ParsedMessageData = JSON.parse(message);

            if (!rawData || rawData.type) {
                ws.send(JSON.stringify({ type: GeneralMessage.BAD_REQUEST, error: "Invalid message" }));
            }

            if (!MessageSchemas[rawData.type]) {
                ws.send(JSON.stringify({ type: GeneralMessage.BAD_REQUEST, error: "Invalid message type" }));
            }

            const validatedData = MessageSchemas[rawData.type].parse(rawData);

            if (validatedData.token) {
                const payload = jwt.verify(validatedData.token, config.system.jwtSecret) as TokenUserPayload;

                if (!payload) {
                    ws.send(JSON.stringify({ type: GeneralMessage.BAD_REQUEST, error: "Invalid token" }));
                }

                session = {
                    ...session,
                    user: payload
                } as AuthenticatedSession;
            } else {
                session = {
                    ...session,
                    name: validatedData.name
                } as UnAuthenticatedSession;
            }

            Socket.meetManager.process(session, ws, validatedData);
        } catch (error) {
            if (error instanceof ZodError) {
                ws.send(JSON.stringify({ type: GeneralMessage.BAD_REQUEST, error: error }));
                return;
            }

            logger.error(error);
            logger.error(`Could not parse message: ${message}`);
        }
    }

    private _handleDisconnection(session: Session) {
        Socket.meetManager.handleDisconnection(session);
    }
}