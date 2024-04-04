import { Request, Response, NextFunction, Express } from "express";
import { v4 as uuidv4 } from "uuid";
import { IRawRequest } from "../types/httpTypes";
import { sessionIdToSocketMap } from "../index";
import logger from "../Logger/logger";
import meetRoutes from "./routes/Meet"

export function startRestServer(
    app: Express,
) {
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

    app.use("/meet", meetRoutes);
}