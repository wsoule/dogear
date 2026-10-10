-- +goose Up
-- PKCE for the native-app sign-in: the one-time code returned through the
-- dogear:// scheme can only be exchanged with the verifier for this challenge,
-- so another app registering the scheme cannot use an intercepted code.
ALTER TABLE mobile_login_codes ADD COLUMN code_challenge TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE mobile_login_codes DROP COLUMN code_challenge;
