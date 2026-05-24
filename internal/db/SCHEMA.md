# Schema

## chat

| Column | Type      | Constraints |
|--------|-----------|-------------|
| id     | BIGSERIAL | PRIMARY KEY |
| info   | TEXT      | NOT NULL    |

## chat_messages

| Column          | Type    | Constraints                                   |
|-----------------|---------|-----------------------------------------------|
| chat_id         | BIGINT  | NOT NULL, PK, FK → chat(id) ON DELETE CASCADE |
| message_id      | BIGINT  | NOT NULL, PK                                  |
| text            | TEXT    | NOT NULL                                      |
| is_myself       | BOOLEAN | NOT NULL                                      |
| reply_to_id     | BIGINT  |                                               |
| reply_to_text   | TEXT    |                                               |
| reply_to_myself | BOOLEAN |                                               |

## chat_members

| Column     | Type    | Constraints                                   |
|------------|---------|-----------------------------------------------|
| chat_id    | BIGINT  | NOT NULL, PK, FK → chat(id) ON DELETE CASCADE |
| user_id    | BIGINT  | NOT NULL, PK                                  |
| username   | TEXT    | NOT NULL                                      |
| first_name | TEXT    | NOT NULL                                      |
| last_name  | TEXT    | NOT NULL                                      |
| is_admin   | BOOLEAN | NOT NULL                                      |
| is_creator | BOOLEAN | NOT NULL                                      |
| rank       | TEXT    | NOT NULL                                      |
