"use strict";

const workspace = document.querySelector(".workspace");
const filters = document.querySelector("#filters");
const refresh = document.querySelector("#refresh");
const back = document.querySelector("#back");
const list = document.querySelector("#thread-list");
const listFeedback = document.querySelector("#list-feedback");
const loadOlder = document.querySelector("#load-older");
const transcript = document.querySelector("#messages");
const transcriptScroll = document.querySelector("#transcript-scroll");
const transcriptTitle = document.querySelector("#transcript-title");
const participants = document.querySelector("#participants");
const transcriptFeedback = document.querySelector("#transcript-feedback");

const threadDate = new Intl.DateTimeFormat(undefined, {month: "short", day: "numeric", hour: "numeric", minute: "2-digit"});
const fullDate = new Intl.DateTimeFormat(undefined, {dateStyle: "medium", timeStyle: "short"});
const dayFormat = new Intl.DateTimeFormat(undefined, {dateStyle: "full"});
const messageTime = new Intl.DateTimeFormat(undefined, {hour: "numeric", minute: "2-digit"});
const statusNames = new Map(
  Array.from(filters.elements.status.options, option => [option.value, option.textContent])
);
const kindNames = {ask: "Ask", reply: "Reply", inform: "Inform"};

let activeFilters = new URLSearchParams();
let threads = new Map();
let selectedId = null;
let nextBefore = null;
let listRequest;
let transcriptRequest;

function element(tag, className, content) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (content !== undefined) node.textContent = content;
  return node;
}

function personName(person) {
  return person.name || person.id;
}

function people(sender, recipient) {
  return personName(sender) + " → " + personName(recipient);
}

function hosts(sender, recipient) {
  return sender.host === recipient.host ? sender.host : sender.host + " → " + recipient.host;
}

function status(value) {
  const label = element("span", "status", statusNames.get(value) || value);
  label.dataset.status = value;
  return label;
}

function timeLabel(seconds, formatter) {
  const date = new Date(seconds * 1000);
  const label = element("time", "", formatter.format(date));
  label.dateTime = date.toISOString();
  label.title = fullDate.format(date);
  return label;
}

function announce(node, message = "", error = false) {
  node.textContent = message;
  node.dataset.error = error;
}

function messageCount(count) {
  return count + (count === 1 ? " message" : " messages");
}

async function getJSON(path, signal) {
  const response = await fetch(path, {signal, cache: "no-store"});
  if (!response.ok) throw new Error(String(response.status));
  return response.json();
}

function threadItem(thread) {
  const item = element("li");
  const button = element("button", "thread");
  button.type = "button";
  button.setAttribute("aria-pressed", String(thread.id === selectedId));
  button.addEventListener("click", () => selectThread(thread.id, true));

  const meta = element("span", "thread-meta");
  meta.append(timeLabel(thread.created, threadDate), status(thread.status), element("span", "", messageCount(thread.count)));
  button.append(
    element("span", "thread-names", people(thread.sender, thread.recipient)),
    element("span", "thread-hosts", hosts(thread.sender, thread.recipient)),
    meta
  );
  item.append(button);
  return {item, button};
}

async function loadThreads(append = false) {
  listRequest?.abort();
  const request = new AbortController();
  listRequest = request;
  const query = new URLSearchParams(activeFilters);
  if (append) query.set("before", nextBefore);
  loadOlder.disabled = true;
  announce(listFeedback, append ? "" : "Loading threads…");
  try {
    const path = query.size ? "/api/conversations?" + query : "/api/conversations";
    const data = await getJSON(path, request.signal);
    if (request.signal.aborted) return;
    if (!append) {
      list.replaceChildren();
      threads = new Map();
    }
    for (const thread of data.threads) {
      if (threads.has(thread.id)) continue;
      const {item, button} = threadItem(thread);
      list.append(item);
      threads.set(thread.id, {thread, button});
    }
    nextBefore = data.next_before;
    loadOlder.hidden = nextBefore === null;
    announce(listFeedback, threads.size ? "" : activeFilters.size ? "No threads match these filters." : "No conversations yet.");
    if (!append) {
      const nextSelection = threads.has(selectedId) ? selectedId : (data.threads[0]?.id ?? null);
      if (nextSelection !== selectedId) workspace.classList.remove("thread-open");
      selectThread(nextSelection);
    }
  } catch (error) {
    if (!request.signal.aborted) {
      announce(listFeedback, append ? "Could not load older threads. Try again." : "Could not load threads. Use Refresh to try again.", true);
    }
  } finally {
    if (!request.signal.aborted) loadOlder.disabled = false;
  }
}

function renderMessages(messages) {
  const byId = new Map(messages.map(message => [message.id, message]));
  const items = new Map();
  const fragment = document.createDocumentFragment();
  let previousDay;
  for (const message of messages) {
    const day = dayFormat.format(new Date(message.created * 1000));
    if (day !== previousDay) {
      fragment.append(element("li", "day", day));
      previousDay = day;
    }

    const item = element("li", "message");
    item.classList.toggle("other", message.sender.id !== messages[0].sender.id);
    item.tabIndex = -1;
    const meta = element("div", "message-meta");
    meta.append(element("strong", "", personName(message.sender)), timeLabel(message.created, messageTime));
    const delivery = element("div", "delivery");
    delivery.append(
      document.createTextNode("To " + personName(message.recipient) + " · " + (kindNames[message.kind] || message.kind) + " · "),
      status(message.status)
    );
    item.append(meta, delivery);
    const replyTo = byId.get(message.in_reply_to);
    if (replyTo) {
      const jump = element("button", "reply-link", "Reply to " + personName(replyTo.sender) + "’s message");
      jump.type = "button";
      jump.append(" at ", timeLabel(replyTo.created, messageTime));
      jump.addEventListener("click", () => {
        const target = items.get(replyTo.id);
        target.scrollIntoView({block: "nearest"});
        target.focus({preventScroll: true});
      });
      item.append(jump);
    }
    const expired = message.body === null;
    item.append(element("p", expired ? "body expired" : "body", expired ? "Body expired" : message.body));
    items.set(message.id, item);
    fragment.append(item);
  }
  transcript.replaceChildren(fragment);
}

async function selectThread(id, open = false) {
  transcriptRequest?.abort();
  const previous = selectedId;
  threads.get(previous)?.button.setAttribute("aria-pressed", "false");
  selectedId = id;
  if (open) workspace.classList.add("thread-open");
  const selected = threads.get(id);
  if (!selected) {
    transcript.replaceChildren();
    transcriptTitle.textContent = "Transcript";
    participants.hidden = true;
    announce(transcriptFeedback, "Select a thread to read its messages.");
    return;
  }

  selected.button.setAttribute("aria-pressed", "true");
  const {sender, recipient} = selected.thread;
  transcriptTitle.textContent = people(sender, recipient);
  participants.textContent = hosts(sender, recipient);
  participants.hidden = false;
  if (open && getComputedStyle(back).display !== "none") {
    window.scrollTo(0, 0);
    transcriptTitle.focus({preventScroll: true});
  }
  if (previous !== id) {
    transcript.replaceChildren();
    transcriptScroll.scrollTop = 0;
  }
  announce(transcriptFeedback, transcript.childElementCount ? "" : "Loading messages…");

  const request = new AbortController();
  transcriptRequest = request;
  try {
    const data = await getJSON("/api/conversations/" + encodeURIComponent(id), request.signal);
    if (request.signal.aborted) return;
    const scrollTop = transcriptScroll.scrollTop;
    renderMessages(data.messages);
    transcriptScroll.scrollTop = scrollTop;
    announce(transcriptFeedback, data.messages.length ? "" : "There are no messages in this thread.");
  } catch (error) {
    if (!request.signal.aborted) {
      announce(transcriptFeedback, "Could not load this transcript. Use Refresh to try again.", true);
    }
  }
}

filters.addEventListener("submit", event => {
  event.preventDefault();
  activeFilters = new URLSearchParams();
  for (const [key, value] of new FormData(filters)) {
    const filter = value.trim();
    if (filter) activeFilters.set(key, filter);
  }
  loadThreads();
});
refresh.addEventListener("click", () => loadThreads());
loadOlder.addEventListener("click", () => loadThreads(true));
back.addEventListener("click", () => {
  workspace.classList.remove("thread-open");
  threads.get(selectedId)?.button.focus();
});

loadThreads();
