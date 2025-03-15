import { Response, Request, NextFunction } from "express";
import { SelectUser } from "../../Schema";

export interface BaseRequest extends Request {
    local: {};
}

export interface AuthenticatedRequest extends BaseRequest {
    local: {
        user: SelectUser;
    };
}

export const successResponse = (
    resObj: Response,
    data: any,
    { statusCode, message } = {
        statusCode: 200,
        message: "success",
    },
): Response<any, Record<string, any>> => {
    return resObj.status(statusCode).json({ message, data });
};

export const failureResponse = (
    resObj: Response,
    data: any,
    { statusCode, error } = {
        statusCode: 500,
        error: "Internal Server Error",
    },
): Response<any, Record<string, any>> => {
    return resObj.status(statusCode).json({ error, data });
};

export type ControllerFunction<T extends Request = BaseRequest> = (
    req: T,
    res: Response,
    next: NextFunction,
) => Promise<any> | any;

export const failureHandler =
    <T extends Request>(controllerFn: ControllerFunction<T>) =>
    (req: Request, res: Response, next: NextFunction) => {
        void Promise.resolve(controllerFn(req as T, res, next));
    };
