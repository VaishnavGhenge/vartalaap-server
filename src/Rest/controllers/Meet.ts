import { NextFunction, Request, Response } from "express";
import { joinMeetQueryParamsSchema } from "../../Types/zod";
import { IRawRequest, IRequest } from "../../Types/httpTypes";
import { meets, sessionIdToMeetMap } from "../../index";
import logger from "../../Logger/logger";
import { v4 as uuidv4 } from "uuid";

export const joinMeet = (req: Request, res: Response, next: NextFunction) => {
    try {
        // Validate request parameters
        const { meetId } = joinMeetQueryParamsSchema.parse(req.query);

        const sessionId = (req as IRequest).sessionId;

        if (!meets.has(meetId)) {
            return res.status(404).json({ message: "Meet not found" });
        }

        logger.info(`User - ${sessionId} joined meet - ${meetId}`);
        return res.status(200).json({ sessionId: sessionId, meetId: meetId });
    } catch (error: any) {
        logger.warn(`Parameters not valid for join-meet request`);

        // Zod validation failed
        return res
            .status(400)
            .json({
                message: "Invalid request parameters",
                error: error.errors,
            });
    }
};

export const createMeet = (
    req: IRawRequest,
    res: Response,
    next: NextFunction,
) => {
    const meetCode: string = uuidv4();
    const sessionId: string = req.sessionId!;

    meets.set(meetCode, {
        peersInMeet: new Set(),
        peersInLobby: new Set([sessionId]),
    });
    sessionIdToMeetMap.set(sessionId, meetCode);

    logger.info(
        `New meet - ${meetCode} created by user - ${(req as any).user.email}`,
    );
    return res.status(201).json({ sessionId: sessionId, meetId: meetCode });
};
