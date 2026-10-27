-- Application Passwords: the optional app_id (a UUID the client picks) WordPress stores with each password.
ALTER TABLE app_passwords ADD COLUMN IF NOT EXISTS app_id text NOT NULL DEFAULT '';
