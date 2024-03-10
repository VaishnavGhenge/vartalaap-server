import { Request, Response, NextFunction, Express } from "express";
import { v4 as uuidv4 } from "uuid";
import { WebSocket } from "ws";
import { IRawRequest, IRequest } from "../types/httpTypes";
import { meets } from "../websocket/server";
import { joinMeetQueryParamsSchema } from "../types/zod";

export function startRestServer(
    app: Express,
    sessionMap: Map<string, WebSocket | null>,
) {
    // Insert sessionId if user request lacks it (means new user)
    app.use((req: IRawRequest, res: Response, next: NextFunction) => {
        if(!req.params.sessionId) {
            const newSessionId = uuidv4();
            sessionMap.set(newSessionId, null);

            req.sessionId = newSessionId;
        } else if(!sessionMap.has(req.params.sessionId)) {
            return res.status(400).json({message: "Invalid session Id"});
        }

        next();
    });

    app.post("/create-meet", (req: Request, res: Response, next: NextFunction) => {
        const meetId: string = uuidv4();
        const sessionId: string = (req as IRequest).sessionId;

        meets.set(meetId, {peersInMeet: new Set(), peersInLobby: new Set([sessionId])});

        return res.status(201).json({ sessionId: sessionId, meetId: meetId });
    });

    app.get("/join-meet", (req, res, next) => {
        try {
            // Validate request parameters
            const { meetId } = joinMeetQueryParamsSchema.parse(req.query);

            const sessionId = (req as IRequest).sessionId;

            if (!meets.has(meetId)) {
                return res.status(404).json({ message: "Meet not found" });
            }

            return res.status(200).json({ sessionId: sessionId, meetId: meetId });
        } catch (error: any) {
            // Zod validation failed
            return res.status(400).json({ message: "Invalid request parameters", error: error.errors });
        }
    });
}