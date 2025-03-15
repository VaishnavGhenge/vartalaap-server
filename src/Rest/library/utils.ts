import { Response, Request, NextFunction } from "express";
import { SelectUser } from "../../Schema";

export interface BaseRequest extends Request {
    local: {};
}

export type TokenUserPayload = Omit<SelectUser, "password">;

export interface AuthenticatedRequest extends BaseRequest {
    local: {
        user: TokenUserPayload;
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
    options?: { statusCode?: number, error?: string },
): Response<any, Record<string, any>> => {
    if (!options) {
        options = {};
    }

    if (!options.statusCode) {
        options.statusCode = 500;
    }

    if (!options.error) {
        options.error = "Internal Server Error";
    }

    return resObj.status(options.statusCode).json({ error: options.error });
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

