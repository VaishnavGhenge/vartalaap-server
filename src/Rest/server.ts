import { Request, Response, NextFunction, Express } from "express";
import { v4 as uuidv4 } from "uuid";
import { IRawRequest } from "../types/httpTypes";
import { sessionIdToSocketMap } from "../index";
import logger from "../Logger/logger";
import meetRoutes from "./routes/Meet"
import userRoutes from "./routes/User";
import express from "express";

export function startRestServer(
    app: Express,
) {

    app.get("", (req, res, next) => {
        return res.status(200).json({"status": "ok"});
    });

    // Middleware to parse JSON request bodies
    app.use(express.json());

    // Insert sessionId if user request lacks it (means new user)
    app.use((req: Request, res: Response, next: NextFunction) => {
        if (!req.params.sessionId) {
            const newSessionId = uuidv4();
            sessionIdToSocketMap.set(newSessionId, null);

            (req as IRawRequest).sessionId = newSessionId;

            logger.info(`First connection by user ${newSessionId}`);
        } else if (!sessionIdToSocketMap.has(req.params.sessionId)) {
            logger.warn(`Invalid session Id declined connection to rest server`);
            return res.status(400).json({ message: "Invalid session Id" });
        }

        next();
    });

    app.use("/meets", meetRoutes);
    app.use("/users", userRoutes);
}