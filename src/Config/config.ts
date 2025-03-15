import dotenv from "dotenv";

dotenv.config();

export const config = {
    db: {
        url: process.env.DATABASE_URL as string,
        host: process.env.POSTGRES_HOST as string,
        port: Number(process.env.POSTGRES_PORT),
        user: process.env.POSTGRES_USER as string,
        password: process.env.POSTGRES_PASSWORD as string,
        database: process.env.POSTGRES_DB as string,
        microservice_url: process.env.DATABASE_MICROSERVICE_URL as string,
    },
    system: {
        port: Number(process.env.PORT),
        host: process.env.HOST as string,
    },
};
