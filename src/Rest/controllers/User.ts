import { Request, Response, NextFunction } from "express";
import bcrypt from "bcrypt";
import jwt from "jsonwebtoken";
import { loginSchema, registerSchema } from "../validators/User";
import { z } from "zod";
import { db } from "../../Config/database";
import { userTable } from "../../Schema";
import { eq } from "drizzle-orm";
import { failureResponse, successResponse } from "../library/utils";
import { config } from "../../Config/config";
import { ValidatedSchema } from "../validators";

export const register = async (
    req: Request,
    res: Response,
    next: NextFunction,
) => {
    const { firstName, lastName, email, password } = req.body as ValidatedSchema<typeof registerSchema>;

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
};

export const login = async (
    req: Request,
    res: Response,
    next: NextFunction,
) => {
    const { email, password } = req.body as ValidatedSchema<typeof loginSchema>;

    // Check if user exists
    const user = await db().query.userTable.findFirst({
        where: eq(userTable.email, email),
    });
    if (!user) {
        return failureResponse(res, null, { error: "User not found", statusCode: 404 });
    }

    // Verify password
    const passwordMatch = await bcrypt.compare(password, user.password);
    if (!passwordMatch) {
        return failureResponse(res, null, { error: "Invalid credentials", statusCode: 401 });
    }

    // Password is correct, generate JWT token
    const token = jwt.sign({ userId: user.id }, config.system.jwtSecret, {
        expiresIn: "1h",
    });

    // Return JWT token
    return successResponse(res, { token });
};
