import { drizzle, PostgresJsDatabase } from "drizzle-orm/postgres-js";
import postgres from "postgres";

import * as schema from "../Schema";
import { sql } from "drizzle-orm";
import { logger } from "../Logger/logger";
import { config } from "./config";

if (!config.db.url) {
    throw new Error("POSTGRES_URL is required.");
}

let queryClient: postgres.Sql<{}> | null = null;
let databaseConnection: PostgresJsDatabase<typeof schema> | null = null;

function createPool(): PostgresJsDatabase<typeof schema> {
    if (!databaseConnection) {
        queryClient = postgres(config.db.url, {
            prepare: false, // Disable prefetch as it is not supported for "Transaction" pool mode
            max: 5, // Limit max connections
            idle_timeout: 20, // Close idle connections after 20 seconds
            connection: {
                application_name: "signaling-server",
                options: `--search_path=public`,
            },
        });

        databaseConnection = drizzle(queryClient, { schema });
    }

    return databaseConnection;
}

const db = (): PostgresJsDatabase<typeof schema> => {
    if (!databaseConnection) {
        return createPool();
    }

    return databaseConnection!;
};

const checkConnection = async () => {
    try {
        const database = db();
        await database.execute(sql`select 1`);
        logger.info(`Connected to database at ${config.db.url}`);
    } catch (error) {
        logger.error(`Failed to connect to database at ${config.db.url}`);
        throw error;
    }
};

void checkConnection();

export { db, checkConnection };
