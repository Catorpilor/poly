-- Lottery ticket: opt-in flag per SL/TP arm. When the ceiling-TP fires (bid
-- >= 0.95 sells the profitable side), and this flag is true, the monitor also
-- attempts a small FOK BUY of the OPPOSITE token at <=$0.05 — capped at $5.
-- A binary market's losing token at <=$0.05 has 20x potential upside if the
-- improbable outcome lands; cheap insurance.
ALTER TABLE sltp_arms
    ADD COLUMN IF NOT EXISTS lottery_ticket_armed BOOLEAN NOT NULL DEFAULT FALSE;
