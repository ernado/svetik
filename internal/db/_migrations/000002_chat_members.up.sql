CREATE TABLE chat_members
(
    chat_id    BIGINT  NOT NULL,
    user_id    BIGINT  NOT NULL,
    username   TEXT    NOT NULL,
    first_name TEXT    NOT NULL,
    last_name  TEXT    NOT NULL,
    is_admin   BOOLEAN NOT NULL,
    is_creator BOOLEAN NOT NULL,
    rank       TEXT    NOT NULL,
    PRIMARY KEY (chat_id, user_id),
    FOREIGN KEY (chat_id) REFERENCES chat (id) ON UPDATE NO ACTION ON DELETE CASCADE
);
