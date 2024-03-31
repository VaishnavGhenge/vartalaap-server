import { Request } from "express";
import { z } from "zod";
import { UserSignupRequestBodySchema } from "./zod";

export interface IRawRequest extends Request {
    sessionId?: string;
}

export interface IRequest extends IRawRequest {
    sessionId: string;
    meetId: string;
    validatedBody?: any;
}

export type UserSignupRequestBody = z.infer<typeof UserSignupRequestBodySchema>;