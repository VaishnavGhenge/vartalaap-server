import { NextFunction, Request, Response } from "express";
import jwt from "jsonwebtoken";

export const checkAuthToken = (req: Request, res: Response, next: NextFunction) => {
    console.log(req.headers);

    const bearerToken = req.headers.authorization as string;
    if (!bearerToken) {
        return res.status(401).json({ message: "Unauthorized" });
    }

    if (bearerToken && bearerToken.startsWith("Bearer ")) {
        // Check if token is valid
        try {
            const token = bearerToken.split(" ")[1];
            const secret = process.env.JWT_SECRET || "secret";

            const user = jwt.verify(token, secret);
            (req as any).user = user;
        } catch (error) {
            return res.status(401).json({ message: "Unauthorized" });
        }
    }

    next();
}