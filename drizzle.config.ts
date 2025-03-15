import { defineConfig } from "drizzle-kit";
import { config } from "./src/config/config";
import { logger } from "./src/Logger/logger";

logger.info(`Connecting to database at ${config.db.microservice_url}`);

export default defineConfig({
    dialect: "postgresql",
    schema: "./src/Schema/*",
    out: "./drizzle",
    dbCredentials: {
        url: config.db.microservice_url,
    },
    // Print all statements
    verbose: true,
    // Always ask for confirmation
    strict: true,
});
