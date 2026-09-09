-- a channel, playlist, single video or link list
create table if not exists sources (
    id         integer primary key,
    kind       text    not null,
    url        text    not null unique,
    title      text,
    added_at   integer not null,
    synced_at  integer
);

create table if not exists items (
    id             integer primary key,
    video_id       text    not null unique,
    source_id      integer references sources(id) on delete set null,
    url            text    not null,
    title          text,
    uploader       text,
    upload_date    text,
    duration       integer,

    state          text    not null default 'pending',
    attempts       integer not null default 0,
    last_error     text,

    format_id      text,
    expected_bytes integer not null default 0,
    bytes_done     integer not null default 0,
    local_path     text,
    local_bytes    integer not null default 0,

    -- a lease, not a bare status: a hard kill would otherwise leave the row
    -- stuck in 'downloading' forever
    lease_owner    text,
    lease_until    integer,

    created_at     integer not null,
    updated_at     integer not null
);

create index if not exists items_state_idx on items(state, id);
create index if not exists items_lease_idx on items(lease_until);

-- one video can travel to several stores, so upload state lives here rather
-- than in an items column
create table if not exists item_targets (
    id          integer primary key,
    item_id     integer not null references items(id) on delete cascade,
    store       text    not null,
    state       text    not null default 'pending',
    attempts    integer not null default 0,
    last_error  text,
    remote_key  text,
    bytes       integer not null default 0,
    lease_owner text,
    lease_until integer,
    updated_at  integer not null,
    unique(item_id, store)
);

create index if not exists item_targets_state_idx on item_targets(state, id);

create table if not exists events (
    id      integer primary key,
    item_id integer,
    ts      integer not null,
    level   text    not null,
    stage   text,
    message text
);

create index if not exists events_item_idx on events(item_id, id);
