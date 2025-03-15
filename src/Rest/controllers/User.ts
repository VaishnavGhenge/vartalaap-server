import { Request, Response, NextFunction } from "express";
import bcrypt from "bcrypt";
import jwt from "jsonwebtoken";
import { loginSchema, registerSchema } from "../validators/User";
import { z } from "zod";
import { db } from "../../Config/database";
import { userTable } from "../../Schema";
import { eq } from "drizzle-orm";
import { successResponse } from "../library/utils";

export const register = async (
    req: Request,
    res: Response,
    next: NextFunction,
) => {
    try {
        const { firstName, lastName, email, password } = registerSchema.parse(
            req.body,
        );

        // Check if user already exists
        const existingUser = await db().query.userTable.findFirst({
            where: eq(userTable.email, email),
        });
        if (existingUser) {
            return res.status(400).json({ error: "User already exists" });
        }

        // Hash the password
        const hashedPassword = await bcrypt.hash(password, 10);

        // Create the user
        const user = await db()
            .insert(userTable)
            .values({
                firstName,
                lastName,
                email,
                password: hashedPassword,
            })
            .returning()
            .execute();

        // Return success response
        return successResponse(res, user, {
            statusCode: 201,
            message: "User registered successfully",
        });
    } catch (error) {
        if (error instanceof z.ZodError) {
            return res.status(400).json({ error: error.message });
        }

        return res.status(500).json({ error: "Internal Server Error" });
    }
};

export const login = async (
    req: Request,
    res: Response,
    next: NextFunction,
) => {
    try {
        const { email, password } = loginSchema.parse(req.body);

        // Check if user exists
        const user = await prisma.user.findUnique({
            where: { email },
        });
        if (!user) {
            return res.status(404).json({ error: "User not found" });
        }

        // Verify password
        const passwordMatch = await bcrypt.compare(password, user.password);
        if (!passwordMatch) {
            return res.status(401).json({ error: "Invalid credentials" });
        }

        const jwtSecret = process.env.JWT_SECRET || "secret";

        // Password is correct, generate JWT token
        const token = jwt.sign({ userId: user.id }, jwtSecret, {
            expiresIn: "1h",
        });

        // Return JWT token
        return res.status(200).json({ user, token });
    } catch (error) {
        if (error instanceof z.ZodError) {
            return res.status(400).json({ error: error.message });
        }

        return res.status(400).json({ error: "Internal Server Error" });
    }
};
