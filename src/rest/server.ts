import { Request, Response, NextFunction, Express } from "express";
import { v4 as uuidv4 } from "uuid";
import { IRawRequest, IRequest } from "../types/httpTypes";
import { meets, sessionIdToSocketMap } from "../websocket/server";
import { UserSignupRequestBodySchema, joinMeetQueryParamsSchema, UserLoginRequestBodySchema } from "../types/zod";
import bcrypt from "bcryptjs";
import { prismaClient } from "../utils/prisma-config";
import logger from "../Logger/logger";
import jwt from "jsonwebtoken";

export function startRestServer(app: Express) {
    // Insert sessionId if user request lacks it (means new user)
    app.use((req: Request, res: Response, next: NextFunction) => {
        if (!req.params.sessionId) {
            const newSessionId = uuidv4();
            sessionIdToSocketMap.set(newSessionId, null);

            (req as IRawRequest).sessionId = newSessionId;
        } else if (!sessionIdToSocketMap.has(req.params.sessionId)) {
            return res.status(400).json({ message: "Invalid session Id" });
        }

        next();
    });

    app.post(
        "/create-meet",
        (req: Request, res: Response, next: NextFunction) => {
            const meetId: string = uuidv4();
            const sessionId: string = (req as IRequest).sessionId;

            meets.set(meetId, {
                peersInMeet: new Set(),
                peersInLobby: new Set([sessionId]),
            });

            return res
                .status(201)
                .json({ sessionId: sessionId, meetId: meetId });
        },
    );

    app.get("/join-meet", (req, res, next) => {
        try {
            // Validate request parameters
            const { meetId } = joinMeetQueryParamsSchema.parse(req.query);

            const sessionId = (req as IRequest).sessionId;

            if (!meets.has(meetId)) {
                return res.status(404).json({ message: "Meet not found" });
            }

            return res
                .status(200)
                .json({ sessionId: sessionId, meetId: meetId });
        } catch (error: any) {
            // Zod validation failed
            return res
                .status(400)
                .json({
                    message: "Invalid request parameters",
                    error: error.errors,
                });
        }
    });

    app.post("/signup", (req: Request, res: Response, next: NextFunction) => {
        try {
            console.log(req.body);
            const validatedBody = UserSignupRequestBodySchema.parse(req.body);
            (req as IRequest).validatedBody = validatedBody;

            next();
        }
        catch(error) {
            logger.error(error);
            return res.status(400).json({ error: "Invalid user data. Please check your input." })
        }
    });

    app.post(
        "/signup",
        async (req: Request, res: Response, next: NextFunction) => {
            try {
                const {email, password, profile_picture, firstname, surname} = (req as IRequest).validatedBody;

                const existingUser = await prismaClient.user.findUnique({
                    where: {
                        email: email,
                    },
                });

                if (existingUser) {
                    return res
                        .status(409)
                        .json({ error: "User already exists" });
                }

                const hashedPassword = await bcrypt.hash(password, 8);

                const newUser = await prismaClient.user.create({
                    data: {
                        email,
                        password: hashedPassword,
                        profile_picture: profile_picture ? profile_picture : "",
                        firstname,
                        surname,
                    },
                });

                return res
                    .status(201)
                    .json({
                        message: "User created successfully",
                        user: newUser,
                    });
            } catch (error) {
                return res.status(500).json({ error: "Internal server error" });
            } finally {
                await prismaClient.$disconnect();
            }
        },
    );

    // app.post("login", () => {})

    app.post("/login", async (req, res) => {
        try {
            const {email, password} = UserLoginRequestBodySchema.parse(req.body);

            const user = await prismaClient.user.findUnique({
                where: {
                    email: email,
                },
            });

            if (!user) {
                return res.status(404).json({ error: "user not found" });
            }

            const passwordMatch = await bcrypt.compare(password, user.password);

            if (!passwordMatch) {
                return res.status(401).json({ error: "invalid password" });
            }

            const token = jwt.sign({userId: user.id}, 'your secret key');
            return res.status(200).json({ message: "login successful", user: user, token: token });
        } catch (error) {
            logger.error(error);
            return res.status(500).json({ error: error });
        } finally {
            await prismaClient.$disconnect();
        }
    });
}

