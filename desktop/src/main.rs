use std::{
    env,
    path::PathBuf,
    process::{Child, Command, Stdio},
    sync::OnceLock,
    thread,
    time::{Duration, Instant},
};

use chrono::{DateTime, Datelike, Local};
use dioxus::desktop::{Config, WindowBuilder};
use dioxus::prelude::*;
use percent_encoding::{NON_ALPHANUMERIC, utf8_percent_encode};
use serde::Deserialize;
use serde::de::DeserializeOwned;
use uuid::Uuid;

static BACKEND: OnceLock<BackendConfig> = OnceLock::new();

#[derive(Clone, Debug)]
struct BackendConfig {
    base_url: String,
    token: Option<String>,
}

#[derive(Default)]
struct Arguments {
    api: Option<String>,
    archive_dir: Option<PathBuf>,
    gmcli: Option<PathBuf>,
    cache: Option<PathBuf>,
}

fn main() {
    let arguments = parse_arguments().unwrap_or_else(|error| {
        eprintln!("gmcli-viewer: {error}");
        std::process::exit(2);
    });
    let (config, mut child) = configure_backend(arguments).unwrap_or_else(|error| {
        eprintln!("gmcli-viewer: {error}");
        std::process::exit(1);
    });
    BACKEND.set(config).expect("backend configured once");
    dioxus::LaunchBuilder::desktop()
        .with_cfg(Config::new().with_window(WindowBuilder::new().with_title("gmcli archive")))
        .launch(App);
    if let Some(child) = child.as_mut() {
        let _ = child.kill();
        let _ = child.wait();
    }
}

fn parse_arguments() -> Result<Arguments, String> {
    let mut parsed = Arguments::default();
    let mut arguments = env::args().skip(1);
    while let Some(argument) = arguments.next() {
        let value = |arguments: &mut std::iter::Skip<std::env::Args>, name: &str| {
            arguments
                .next()
                .ok_or_else(|| format!("{name} requires a value"))
        };
        match argument.as_str() {
            "--api" => parsed.api = Some(value(&mut arguments, "--api")?),
            "--archive-dir" => {
                parsed.archive_dir = Some(value(&mut arguments, "--archive-dir")?.into())
            }
            "--gmcli" => parsed.gmcli = Some(value(&mut arguments, "--gmcli")?.into()),
            "--cache" => parsed.cache = Some(value(&mut arguments, "--cache")?.into()),
            "-h" | "--help" => {
                println!(
                    "gmcli-viewer [--archive-dir PATH] [--gmcli PATH] [--cache PATH]\n\
                     gmcli-viewer --api URL\n\n\
                     With --api, connect to an existing archive API. Otherwise, start a private\n\
                     loopback gmcli archive server for the selected JSONL archive."
                );
                std::process::exit(0);
            }
            _ => return Err(format!("unknown argument {argument:?}; try --help")),
        }
    }
    Ok(parsed)
}

fn configure_backend(arguments: Arguments) -> Result<(BackendConfig, Option<Child>), String> {
    if let Some(api) = arguments.api {
        return Ok((
            BackendConfig {
                base_url: api.trim_end_matches('/').to_owned(),
                token: env::var("GMCLI_ARCHIVE_API_TOKEN").ok(),
            },
            None,
        ));
    }
    let archive_dir = arguments
        .archive_dir
        .or_else(|| env::var_os("GMCLI_ARCHIVE_DIR").map(PathBuf::from))
        .or_else(default_archive_dir)
        .ok_or_else(|| "provide --archive-dir or set GMCLI_ARCHIVE_DIR".to_owned())?;
    let gmcli = arguments
        .gmcli
        .or_else(|| env::var_os("GMCLI_BIN").map(PathBuf::from))
        .unwrap_or_else(|| "gmcli".into());
    let probe = std::net::TcpListener::bind("127.0.0.1:0")
        .map_err(|error| format!("choose backend port: {error}"))?;
    let port = probe
        .local_addr()
        .map_err(|error| error.to_string())?
        .port();
    drop(probe);
    let token = Uuid::new_v4().simple().to_string();
    let address = format!("127.0.0.1:{port}");
    let mut command = Command::new(&gmcli);
    command.arg("archive").arg("--dir").arg(&archive_dir);
    if let Some(cache) = arguments.cache {
        command.arg("--cache").arg(cache);
    }
    let mut child = command
        .arg("serve")
        .arg("--listen")
        .arg(&address)
        .env("GMCLI_ARCHIVE_API_TOKEN", &token)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::inherit())
        .spawn()
        .map_err(|error| format!("start {}: {error}", gmcli.display()))?;
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        if std::net::TcpStream::connect(&address).is_ok() {
            break;
        }
        if let Some(status) = child.try_wait().map_err(|error| error.to_string())? {
            return Err(format!("archive API exited during startup with {status}"));
        }
        if Instant::now() >= deadline {
            let _ = child.kill();
            return Err("timed out waiting for the archive API".to_owned());
        }
        thread::sleep(Duration::from_millis(100));
    }
    Ok((
        BackendConfig {
            base_url: format!("http://{address}"),
            token: Some(token),
        },
        Some(child),
    ))
}

fn default_archive_dir() -> Option<PathBuf> {
    let home = env::var_os("HOME")?;
    let candidate = PathBuf::from(home).join("Backups/gmcli/git-sync/archive");
    candidate.is_dir().then_some(candidate)
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
struct Meta {
    exported_at: String,
    conversations: usize,
    messages: usize,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
struct Participant {
    name: String,
    e164: String,
    formatted_number: String,
    is_me: bool,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
struct Conversation {
    conversation_id: String,
    name: String,
    is_group: bool,
    participants: Vec<Participant>,
    last_message_time_ms: i64,
    message_count: usize,
    #[serde(default)]
    preview: String,
}

impl Conversation {
    fn label(&self) -> String {
        if !self.name.trim().is_empty() {
            return self.name.clone();
        }
        let names: Vec<&str> = self
            .participants
            .iter()
            .filter(|participant| !participant.is_me)
            .filter_map(|participant| {
                [
                    &participant.name,
                    &participant.formatted_number,
                    &participant.e164,
                ]
                .into_iter()
                .find(|value| !value.trim().is_empty())
                .map(String::as_str)
            })
            .collect();
        if names.is_empty() {
            "Untitled conversation".to_owned()
        } else {
            names.join(", ")
        }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
struct ConversationPage {
    conversations: Vec<Conversation>,
    total: usize,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
struct Message {
    message_id: String,
    #[serde(default)]
    sender_name: String,
    body: Option<String>,
    timestamp_ms: i64,
    is_from_me: bool,
    mime_type: Option<String>,
}

#[derive(Clone, Debug, Deserialize, PartialEq)]
struct MessagePage {
    conversation: Conversation,
    messages: Vec<Message>,
    has_older: bool,
    before_cursor: Option<String>,
}

#[derive(Clone, Debug, Deserialize)]
struct ApiError {
    error: String,
}

async fn api_get<T: DeserializeOwned>(path: &str, query: &[(&str, String)]) -> Result<T, String> {
    let config = BACKEND.get().expect("backend configured");
    let client = reqwest::Client::new();
    let mut request = client
        .get(format!("{}{}", config.base_url, path))
        .query(query);
    if let Some(token) = &config.token {
        request = request.bearer_auth(token);
    }
    let response = request.send().await.map_err(|error| error.to_string())?;
    let status = response.status();
    if !status.is_success() {
        let error = response
            .json::<ApiError>()
            .await
            .map(|value| value.error)
            .unwrap_or_else(|_| status.to_string());
        return Err(error);
    }
    response.json().await.map_err(|error| error.to_string())
}

#[component]
fn App() -> Element {
    let mut conversations = use_signal(|| None::<ConversationPage>);
    let mut selected_id = use_signal(|| None::<String>);
    let mut messages = use_signal(|| None::<MessagePage>);
    let mut filter_draft = use_signal(String::new);
    let mut filter = use_signal(String::new);
    let mut sort = use_signal(|| "recent".to_owned());
    let mut loading_conversations = use_signal(|| true);
    let mut loading_messages = use_signal(|| false);
    let mut loading_older = use_signal(|| false);
    let mut error = use_signal(|| None::<String>);
    let metadata = use_resource(|| async { api_get::<Meta>("/api/v1/meta", &[]).await });

    use_effect(move || {
        let query = filter();
        let order = sort();
        let request_query = query.clone();
        let request_order = order.clone();
        loading_conversations.set(true);
        spawn(async move {
            let result = api_get::<ConversationPage>(
                "/api/v1/conversations",
                &[
                    ("query", query),
                    ("sort", order),
                    ("limit", "500".to_owned()),
                ],
            )
            .await;
            if filter() != request_query || sort() != request_order {
                return;
            }
            match result {
                Ok(page) => {
                    let selection_is_visible = selected_id().is_some_and(|selected| {
                        page.conversations
                            .iter()
                            .any(|conversation| conversation.conversation_id == selected)
                    });
                    if !selection_is_visible {
                        selected_id.set(
                            page.conversations
                                .first()
                                .map(|value| value.conversation_id.clone()),
                        );
                    }
                    conversations.set(Some(page));
                    error.set(None);
                }
                Err(message) => error.set(Some(message)),
            }
            loading_conversations.set(false);
        });
    });

    use_effect(move || {
        let Some(id) = selected_id() else {
            messages.set(None);
            return;
        };
        let request_id = id.clone();
        loading_messages.set(true);
        loading_older.set(false);
        messages.set(None);
        spawn(async move {
            match api_get::<MessagePage>(
                &format!("/api/v1/conversations/{}/messages", path_segment(&id)),
                &[("limit", "200".to_owned())],
            )
            .await
            {
                Ok(page) => {
                    if selected_id().as_deref() == Some(request_id.as_str()) {
                        messages.set(Some(page));
                        error.set(None);
                    }
                }
                Err(message) => {
                    if selected_id().as_deref() == Some(request_id.as_str()) {
                        error.set(Some(message));
                    }
                }
            }
            if selected_id().as_deref() == Some(request_id.as_str()) {
                loading_messages.set(false);
            }
        });
    });

    let selected = selected_id();
    rsx! {
        document::Title { "gmcli archive" }
        style { {include_str!("../assets/main.css")} }
        div { class: "app-shell",
            aside { class: "sidebar",
                header { class: "sidebar-header",
                    div { class: "brand-row",
                        div { class: "mark", "g" }
                        div {
                            h1 { "Messages" }
                            match metadata.read().as_ref() {
                                Some(Ok(meta)) => rsx! { p { "{meta.conversations} conversations · {compact_count(meta.messages)} messages" } },
                                Some(Err(_)) => rsx! { p { "Local JSONL archive" } },
                                None => rsx! { p { "Opening local archive…" } },
                            }
                        }
                    }
                    form {
                        class: "search-form",
                        onsubmit: move |event| {
                            event.prevent_default();
                            filter.set(filter_draft());
                        },
                        input {
                            r#type: "search",
                            value: "{filter_draft}",
                            placeholder: "Filter conversations",
                            oninput: move |event| filter_draft.set(event.value()),
                        }
                    }
                    div { class: "sort-control", role: "group", aria_label: "Conversation order",
                        button {
                            class: if sort() == "recent" { "active" } else { "" },
                            onclick: move |_| sort.set("recent".to_owned()),
                            "Recent"
                        }
                        button {
                            class: if sort() == "messages" { "active" } else { "" },
                            onclick: move |_| sort.set("messages".to_owned()),
                            "Most messages"
                        }
                    }
                }
                div { class: "conversation-list",
                    if loading_conversations() && conversations().is_none() {
                        div { class: "empty-state", "Loading conversations…" }
                    }
                    if let Some(page) = conversations() {
                        for conversation in page.conversations {
                            ConversationRow {
                                key: "{conversation.conversation_id}",
                                conversation: conversation.clone(),
                                selected: selected.as_ref() == Some(&conversation.conversation_id),
                                on_select: move |id| selected_id.set(Some(id)),
                            }
                        }
                        if page.total == 0 {
                            div { class: "empty-state", "No conversations match." }
                        }
                    }
                }
            }
            main { class: "thread-pane",
                if let Some(message) = error() {
                    div { class: "error-banner", "{message}" }
                }
                if loading_messages() {
                    div { class: "thread-empty", div { class: "spinner" } "Loading messages…" }
                } else if let Some(page) = messages() {
                    Thread {
                        key: "{page.conversation.conversation_id}",
                        page,
                        loading_older: loading_older(),
                        on_load_older: move |(id, cursor): (String, String)| {
                            let request_id = id.clone();
                            loading_older.set(true);
                            spawn(async move {
                                let result = api_get::<MessagePage>(&format!("/api/v1/conversations/{}/messages", path_segment(&id)), &[
                                    ("before", cursor), ("limit", "200".to_owned()),
                                ]).await;
                                if selected_id().as_deref() != Some(request_id.as_str()) {
                                    return;
                                }
                                match result {
                                    Ok(older) => {
                                        if let Some(mut current) = messages()
                                            && current.conversation.conversation_id == request_id
                                        {
                                            let mut combined = older.messages;
                                            combined.append(&mut current.messages);
                                            current.messages = combined;
                                            current.has_older = older.has_older;
                                            current.before_cursor = older.before_cursor;
                                            messages.set(Some(current));
                                        }
                                    }
                                    Err(message) => error.set(Some(message)),
                                }
                                loading_older.set(false);
                            });
                        }
                    }
                } else {
                    div { class: "thread-empty",
                        div { class: "empty-icon", "⌁" }
                        h2 { "Your message archive" }
                        p { "Choose a conversation to browse its complete local history." }
                    }
                }
            }
        }
    }
}

#[component]
fn ConversationRow(
    conversation: Conversation,
    selected: bool,
    on_select: EventHandler<String>,
) -> Element {
    let id = conversation.conversation_id.clone();
    let label = conversation.label();
    let initial = label
        .chars()
        .find(|character| character.is_alphanumeric())
        .unwrap_or('?')
        .to_uppercase()
        .to_string();
    rsx! {
        button {
            class: if selected { "conversation-row selected" } else { "conversation-row" },
            onclick: move |_| on_select.call(id.clone()),
            div { class: "avatar", "{initial}" }
            div { class: "conversation-copy",
                div { class: "conversation-title-row",
                    strong { "{label}" }
                    time { "{short_time(conversation.last_message_time_ms)}" }
                }
                div { class: "conversation-preview-row",
                    span { "{conversation.preview}" }
                    small { "{compact_count(conversation.message_count)}" }
                }
            }
        }
    }
}

#[component]
fn Thread(
    page: MessagePage,
    loading_older: bool,
    on_load_older: EventHandler<(String, String)>,
) -> Element {
    let title = page.conversation.label();
    let subtitle = participant_summary(&page.conversation);
    let load_id = page.conversation.conversation_id.clone();
    let load_cursor = page.before_cursor.clone();
    rsx! {
        header { class: "thread-header",
            div { class: "avatar thread-avatar", "{title.chars().next().unwrap_or('?').to_uppercase()}" }
            div {
                h2 { "{title}" }
                p { "{subtitle}" }
            }
            span { class: "message-total", "{compact_count(page.conversation.message_count)} messages" }
        }
        section {
            class: "messages",
            aria_label: "Message history",
            onmounted: move |_| {
                document::eval(
                    r#"const messages = document.querySelector('.messages');
                    if (messages) {
                        let previousHeight = -1;
                        let stableFrames = 0;
                        let attempts = 0;
                        const scroll = () => {
                            messages.scrollTop = messages.scrollHeight;
                            if (messages.scrollHeight === previousHeight) stableFrames += 1;
                            else stableFrames = 0;
                            previousHeight = messages.scrollHeight;
                            if (stableFrames < 3 && attempts++ < 20) setTimeout(scroll, 50);
                        };
                        scroll();
                    }"#,
                );
            },
            if page.has_older {
                button {
                    class: "load-older",
                    disabled: loading_older,
                    onclick: move |_| {
                        if let Some(cursor) = load_cursor.clone() {
                            on_load_older.call((load_id.clone(), cursor));
                        }
                    },
                    if loading_older { "Loading…" } else { "Load older messages" }
                }
            } else {
                div { class: "history-start", "Beginning of archived history" }
            }
            for message in page.messages {
                MessageBubble { key: "{message.message_id}", message }
            }
        }
    }
}

#[component]
fn MessageBubble(message: Message) -> Element {
    let body = message.body.clone().unwrap_or_else(|| {
        message
            .mime_type
            .as_ref()
            .map(|mime| format!("Attachment · {mime}"))
            .unwrap_or_else(|| "Empty message".to_owned())
    });
    let sender = if message.is_from_me {
        "You".to_owned()
    } else if message.sender_name.is_empty() {
        "Unknown sender".to_owned()
    } else {
        message.sender_name.clone()
    };
    rsx! {
        article { class: if message.is_from_me { "message-row from-me" } else { "message-row" },
            div { class: "message-meta",
                span { "{sender}" }
                time { title: "{full_time(message.timestamp_ms)}", "{message_time(message.timestamp_ms)}" }
            }
            div { class: "bubble", "{body}" }
        }
    }
}

fn participant_summary(conversation: &Conversation) -> String {
    let values: Vec<String> = conversation
        .participants
        .iter()
        .filter(|value| !value.is_me)
        .filter_map(|value| {
            [&value.name, &value.formatted_number, &value.e164]
                .into_iter()
                .find(|text| !text.trim().is_empty())
                .cloned()
        })
        .collect();
    if values.is_empty() {
        conversation.conversation_id.clone()
    } else {
        values.join(" · ")
    }
}

fn short_time(timestamp_ms: i64) -> String {
    let Some(when) =
        DateTime::from_timestamp_millis(timestamp_ms).map(|value| value.with_timezone(&Local))
    else {
        return "".to_owned();
    };
    let now = Local::now();
    if when.date_naive() == now.date_naive() {
        when.format("%-I:%M %p").to_string()
    } else if when.year() == now.year() {
        when.format("%b %-d").to_string()
    } else {
        when.format("%Y").to_string()
    }
}

fn message_time(timestamp_ms: i64) -> String {
    DateTime::from_timestamp_millis(timestamp_ms)
        .map(|value| {
            value
                .with_timezone(&Local)
                .format("%b %-d, %-I:%M %p")
                .to_string()
        })
        .unwrap_or_default()
}

fn full_time(timestamp_ms: i64) -> String {
    DateTime::from_timestamp_millis(timestamp_ms)
        .map(|value| {
            value
                .with_timezone(&Local)
                .format("%A, %B %-d, %Y at %-I:%M:%S %p %Z")
                .to_string()
        })
        .unwrap_or_default()
}

fn compact_count(count: usize) -> String {
    if count >= 1_000_000 {
        format!("{:.1}m", count as f64 / 1_000_000.0)
    } else if count >= 10_000 {
        format!("{:.1}k", count as f64 / 1_000.0)
    } else {
        count.to_string()
    }
}

fn path_segment(value: &str) -> String {
    utf8_percent_encode(value, NON_ALPHANUMERIC).to_string()
}
