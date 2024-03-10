import { Request } from "express";

export interface IRawRequest extends Request {
    sessionId?: string;
}

export interface IRequest extends IRawRequest {
    sessionId: string;
    meetId: string;
}