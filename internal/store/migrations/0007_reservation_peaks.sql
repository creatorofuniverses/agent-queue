-- Highest memory a reservation's nested tasks used at once, recorded when the
-- reservation stops being held; NULL for reservations that ended before it was
-- recorded (unknown, not zero). The stand's peak goes to memory_peak.
ALTER TABLE items ADD COLUMN task_memory_peak INTEGER;
