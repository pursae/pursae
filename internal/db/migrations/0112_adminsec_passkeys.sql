-- Pursae Security: passkeys (WebAuthn). Expand-only: three new tables, nothing existing changes, so
-- the previous release runs on this schema (it never reads them).

-- The WebAuthn user handle of an account: 32 random bytes, never the user id, so a passkey reveals
-- nothing about the account it belongs to. Created with the user's first passkey.
CREATE TABLE IF NOT EXISTS adminsec_passkey_users (
    user_id    bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    handle     bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One row per registered passkey (public key credential). Only the public key is stored.
CREATE TABLE IF NOT EXISTS adminsec_passkeys (
    id               bigserial PRIMARY KEY,
    user_id          bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    credential_id    bytea NOT NULL UNIQUE,
    public_key       bytea NOT NULL,                       -- COSE key
    name             text NOT NULL DEFAULT '',             -- chosen by the user
    attestation_type text NOT NULL DEFAULT '',
    aaguid           bytea,
    transports       text[] NOT NULL DEFAULT '{}',
    sign_count       bigint NOT NULL DEFAULT 0,            -- last signature counter (clone detection)
    clone_warning    boolean NOT NULL DEFAULT false,       -- the counter went backwards: the passkey is refused
    user_verified    boolean NOT NULL DEFAULT false,
    backup_eligible  boolean NOT NULL DEFAULT false,
    backup_state     boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_used_at     timestamptz
);
CREATE INDEX IF NOT EXISTS adminsec_passkeys_user ON adminsec_passkeys (user_id);

-- A WebAuthn ceremony in progress: the challenge and what it allows (the library's session data).
-- Single use: a ceremony is deleted when it is answered, right or wrong; it expires after 5 minutes.
-- purpose: register (user_id is the account), login (passkey sign-in, no account yet), second (the
-- second step of a password login; parent_id is its adminsec_challenges row).
CREATE TABLE IF NOT EXISTS adminsec_passkey_ceremonies (
    id         bigserial PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE,
    purpose    text NOT NULL,
    user_id    bigint REFERENCES users(id) ON DELETE CASCADE,
    parent_id  bigint,
    ip         text NOT NULL DEFAULT '',
    session    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS adminsec_passkey_ceremonies_ip ON adminsec_passkey_ceremonies (ip, created_at);
