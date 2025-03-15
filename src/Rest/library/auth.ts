import { Request, Response, NextFunction } from "express";
import { AuthenticatedRequest, failureResponse, TokenUserPayload } from "./utils";
import jwt from "jsonwebtoken";
import { config } from "../../Config/config";
import { db } from "../../Config/database";
import { and, eq } from "drizzle-orm";
import { userTable } from "../../Schema";
import { logger } from "../../Logger/logger";

export const authenticate = async (req: Request, res: Response, next: NextFunction) => {
    const { token } = req.headers as { token: string };

    if (!token) {
        return failureResponse(res, { error: "Token not found" });
    }

    const user = jwt.verify(token, config.system.jwtSecret) as TokenUserPayload;

    if (user === undefined) {
        return failureResponse(res, { error: "Token not found" });
    }

    if (!await isUserExists({ id: user.id, email: user.email })) {
        return failureResponse(res, { error: "Invalid authentication token", statusCode: 401 });
    }

    if ((req as AuthenticatedRequest).local === undefined) {
        (req as AuthenticatedRequest).local = { user };
    } else {
        (req as AuthenticatedRequest).local.user = user;
    }

    next();
};

export const isUserExists = async ({ id, email }: { id: string; email: string }): Promise<boolean> => {
    try {
        const user = await db().query.userTable.findFirst({
            where: and(eq(userTable.id, id), eq(userTable.email, email)),
            columns: {
                id: true,
            },
        });

        return user !== undefined;
    } catch (error) {
        logger.error(error);
        return false;
    }
};