import { pgTable, uuid, varchar } from "drizzle-orm/pg-core";
import { userTable } from "./index";
import { timestamp } from "drizzle-orm/pg-core";

export const meetTable = pgTable("meet", {
    id: uuid("id").defaultRandom().primaryKey(),
    meetCode: varchar("meet_code", { length: 255 }).unique(),
    created_by: uuid("created_by")
        .references(() => userTable.id, { onDelete: "cascade" })
        .notNull(),
    createdAt: timestamp("created_at", { withTimezone: true })
        .notNull()
        .defaultNow(),
    updatedAt: timestamp("updated_at")
        .notNull()
        .$onUpdate(() => new Date()),
});

export type InsertMeet = typeof meetTable.$inferInsert;
export type SelectMeet = typeof meetTable.$inferSelect;

export const participantTable = pgTable("participant", {
    id: uuid("id").defaultRandom().primaryKey(),
    userId: uuid("user_id")
        .references(() => userTable.id, { onDelete: "cascade" })
        .notNull(),
    createdAt: timestamp("created_at", { withTimezone: true })
        .notNull()
        .defaultNow(),
    updatedAt: timestamp("updated_at")
        .notNull()
        .$onUpdate(() => new Date()),
});

export type InsertParticipant = typeof participantTable.$inferInsert;
export type SelectParticipant = typeof participantTable.$inferSelect;
