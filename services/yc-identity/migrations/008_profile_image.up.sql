-- OAuth profile image URL (phone_e164 already exists on users).
ALTER TABLE users ADD COLUMN IF NOT EXISTS profile_image_url TEXT;
