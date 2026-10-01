CREATE TABLE iam.users (
    id UUID PRIMARY KEY,
    email TEXT NOT NULL UNIQUE
);
