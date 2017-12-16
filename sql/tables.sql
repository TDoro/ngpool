DROP TABLE IF EXISTS share CASCADE;
DROP TABLE IF EXISTS block CASCADE;
DROP TABLE IF EXISTS payout_address CASCADE;
DROP TABLE IF EXISTS credit CASCADE;
DROP TABLE IF EXISTS users CASCADE;

CREATE TABLE share
(
    username varchar NOT NULL,
    difficulty double precision NOT NULL,
    mined_at timestamp NOT NULL,
    sharechain varchar NOT NULL,
    currency varchar NOT NULL
);

CREATE TABLE block
(
    currency varchar NOT NULL,
    powalgo varchar NOT NULL,
    height bigint NOT NULL,
    hash varchar NOT NULL,
    powhash varchar NOT NULL,
    subsidy numeric NOT NULL,
    mined_at timestamp NOT NULL,
    mined_by varchar NOT NULL,
    difficulty double precision NOT NULL,
    credited boolean NOT NULL DEFAULT false,
    mature boolean DEFAULT false NOT NULL,
    payout_data json DEFAULT '{}'::JSON NOT NULL,
    CONSTRAINT block_pkey PRIMARY KEY (hash)
);

CREATE TABLE users
(
    id SERIAL NOT NULL,
    username varchar,
    password varchar,
    email varchar NOT NULL,
    verified_email boolean NOT NULL DEFAULT false,
    tfa_code varchar,
    tfa_enabled boolean NOT NULL DEFAULT false,
    CONSTRAINT users_pkey PRIMARY KEY (id),
    CONSTRAINT unique_email UNIQUE (email),
    CONSTRAINT unique_username UNIQUE (username)
);

CREATE TABLE payout_address
(
    user_id integer NOT NULL,
    currency varchar,
    address varchar,
    CONSTRAINT user_id_fk FOREIGN KEY (user_id)
        REFERENCES users (id) MATCH SIMPLE
        ON UPDATE NO ACTION
        ON DELETE NO ACTION,
    CONSTRAINT payout_address_pkey PRIMARY KEY (user_id, currency)
);

CREATE TABLE credit
(
    id SERIAL NOT NULL,
    user_id integer NOT NULL,
    amount numeric NOT NULL,
    currency varchar NOT NULL,
    blockhash varchar NOT NULL,
    sharechain varchar NOT NULL,
    CONSTRAINT unique_credit UNIQUE (user_id, blockhash),
    CONSTRAINT blockhash_fk FOREIGN KEY (blockhash)
        REFERENCES block (hash) MATCH SIMPLE
        ON UPDATE NO ACTION
        ON DELETE NO ACTION,
    CONSTRAINT user_id_fk FOREIGN KEY (user_id)
        REFERENCES users (id) MATCH SIMPLE
        ON UPDATE NO ACTION
        ON DELETE NO ACTION,
    CONSTRAINT credit_pkey PRIMARY KEY (id)
);
