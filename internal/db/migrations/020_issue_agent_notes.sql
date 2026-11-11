-- «Указания агенту» of a task, saved as the developer types (like review_focus of an MR). Not touched by syncs;
-- every run of the task takes them as its notes when it starts.
ALTER TABLE issues ADD COLUMN agent_notes TEXT NOT NULL DEFAULT '';
