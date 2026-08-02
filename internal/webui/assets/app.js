const STORAGE_KEY = "vertex-chat-state-v1";
const DEFAULT_SYSTEM = "You are a thoughtful, precise assistant. Use the conversation history for context. Give clear, useful answers and say when you are uncertain.";
const MAX_CONTEXT_MESSAGES = 40;

// crypto.randomUUID() only exists in secure contexts (HTTPS or localhost) —
// on plain HTTP it's simply undefined, which used to throw here and silently
// break every send. These IDs are just local bookkeeping keys, not security
// tokens, so crypto.getRandomValues (no secure-context restriction) is a
// perfectly good fallback.
function generateId() {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

const elements = {
  conversation: document.querySelector("#conversation"),
  messages: document.querySelector("#messages"),
  welcome: document.querySelector("#welcome"),
  form: document.querySelector("#composer"),
  prompt: document.querySelector("#prompt"),
  send: document.querySelector("#send-button"),
  stop: document.querySelector("#stop-button"),
  history: document.querySelector("#history-list"),
  historyCount: document.querySelector("#history-count"),
  model: document.querySelector("#model-name"),
  sidebar: document.querySelector("#sidebar"),
  scrim: document.querySelector("#sidebar-scrim"),
  dialog: document.querySelector("#settings-dialog"),
  systemPrompt: document.querySelector("#system-prompt"),
  temperature: document.querySelector("#temperature"),
  temperatureValue: document.querySelector("#temperature-value"),
  toast: document.querySelector("#toast"),
};

let state = loadState();
let appConfig = { model: "gemini-3.5-flash", max_messages: 100, mock: false };
let activeController = null;
let toastTimer = null;

function loadState() {
  const fallback = {
    chats: [],
    activeId: null,
    preferences: { system: DEFAULT_SYSTEM, temperature: 0.7, theme: "light" },
  };
  try {
    const parsed = JSON.parse(localStorage.getItem(STORAGE_KEY));
    return {
      chats: Array.isArray(parsed?.chats) ? parsed.chats : [],
      activeId: parsed?.activeId || null,
      preferences: { ...fallback.preferences, ...(parsed?.preferences || {}) },
    };
  } catch {
    return fallback;
  }
}

function saveState() {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
}

function activeChat() {
  return state.chats.find((chat) => chat.id === state.activeId) || null;
}

function createChat() {
  const now = new Date().toISOString();
  const chat = { id: generateId(), title: "New conversation", messages: [], createdAt: now, updatedAt: now };
  state.chats.unshift(chat);
  state.activeId = chat.id;
  saveState();
  render();
  elements.prompt.focus();
  closeSidebar();
  return chat;
}

function selectChat(id) {
  if (activeController) activeController.abort();
  state.activeId = id;
  saveState();
  render();
  closeSidebar();
}

function deleteChat(id) {
  state.chats = state.chats.filter((chat) => chat.id !== id);
  if (state.activeId === id) state.activeId = state.chats[0]?.id || null;
  saveState();
  render();
}

function clearCurrentChat() {
  const chat = activeChat();
  if (!chat || chat.messages.length === 0) return;
  chat.messages = [];
  chat.title = "New conversation";
  chat.updatedAt = new Date().toISOString();
  saveState();
  render();
  showToast("Conversation cleared");
}

function render() {
  renderHistory();
  renderMessages();
  document.documentElement.dataset.theme = state.preferences.theme;
}

function renderHistory() {
  elements.history.replaceChildren();
  elements.historyCount.textContent = String(state.chats.length);
  const chats = [...state.chats].sort((a, b) => String(b.updatedAt).localeCompare(String(a.updatedAt)));
  for (const chat of chats) {
    const item = document.createElement("div");
    item.className = `history-item${chat.id === state.activeId ? " active" : ""}`;

    const title = document.createElement("button");
    title.className = "history-title";
    title.type = "button";
    title.textContent = chat.title;
    title.addEventListener("click", () => selectChat(chat.id));
    const remove = document.createElement("button");
    remove.className = "history-delete";
    remove.type = "button";
    remove.ariaLabel = `Delete ${chat.title}`;
    remove.textContent = "×";
    remove.addEventListener("click", () => deleteChat(chat.id));
    item.append(title, remove);
    elements.history.append(item);
  }
}

function renderMessages() {
  elements.messages.replaceChildren();
  const messages = activeChat()?.messages || [];
  elements.welcome.classList.toggle("hidden", messages.length > 0);
  elements.messages.classList.toggle("hidden", messages.length === 0);
  for (const message of messages) elements.messages.append(messageElement(message));
  requestAnimationFrame(scrollToBottom);
}

function messageElement(message) {
  const article = document.createElement("article");
  article.className = `message ${message.role}`;
  article.dataset.messageId = message.id;

  const avatar = document.createElement("div");
  avatar.className = "message-avatar";
  avatar.textContent = message.role === "user" ? "YOU" : "✦";

  const body = document.createElement("div");
  body.className = "message-body";
  const meta = document.createElement("div");
  meta.className = "message-meta";
  meta.textContent = message.role === "user" ? "You" : appConfig.model;
  const content = document.createElement("div");
  content.className = `message-content${message.streaming ? " typing-cursor" : ""}`;
  renderRichText(content, message.content || "");
  body.append(meta, content);

  if (!message.streaming && message.content) {
    const actions = document.createElement("div");
    actions.className = "message-actions";
    const copy = document.createElement("button");
    copy.className = "message-action";
    copy.type = "button";
    copy.textContent = "Copy";
    copy.addEventListener("click", async () => {
      await navigator.clipboard.writeText(message.content);
      showToast("Copied to clipboard");
    });
    actions.append(copy);
    if (message.role === "assistant") {
      const retry = document.createElement("button");
      retry.className = "message-action";
      retry.type = "button";
      retry.textContent = "Regenerate";
      retry.addEventListener("click", regenerate);
      actions.append(retry);
    }
    body.append(actions);
  }

  article.append(avatar, body);
  return article;
}

function renderRichText(container, text) {
  container.replaceChildren();
  const sections = text.split("```");
  sections.forEach((section, index) => {
    if (index % 2 === 1) {
      const pre = document.createElement("pre");
      const code = document.createElement("code");
      const firstBreak = section.indexOf("\n");
      code.textContent = firstBreak > -1 ? section.slice(firstBreak + 1) : section;
      pre.append(code);
      container.append(pre);
      return;
    }
    renderBlocks(container, section);
  });
}

// Block-level markdown: headings, ordered/unordered lists, blockquotes, and
// paragraphs (everything else falls through to a paragraph). Code fences are
// already stripped out by renderRichText before this ever sees the text.
function renderBlocks(container, text) {
  const lines = text.split("\n");
  let paragraphLines = [];

  const flushParagraph = () => {
    if (!paragraphLines.length) return;
    const p = document.createElement("p");
    appendInlineText(p, paragraphLines.join(" "));
    container.append(p);
    paragraphLines = [];
  };

  let i = 0;
  while (i < lines.length) {
    const line = lines[i];

    if (!line.trim()) {
      flushParagraph();
      i++;
      continue;
    }

    const heading = line.match(/^(#{1,6})\s+(.*)$/);
    if (heading) {
      flushParagraph();
      const h = document.createElement(`h${heading[1].length}`);
      appendInlineText(h, heading[2]);
      container.append(h);
      i++;
      continue;
    }

    if (/^>\s?/.test(line)) {
      flushParagraph();
      const quoteLines = [];
      while (i < lines.length && /^>\s?/.test(lines[i])) {
        quoteLines.push(lines[i].replace(/^>\s?/, ""));
        i++;
      }
      const blockquote = document.createElement("blockquote");
      appendInlineText(blockquote, quoteLines.join(" "));
      container.append(blockquote);
      continue;
    }

    if (/^[-*]\s+/.test(line)) {
      flushParagraph();
      const ul = document.createElement("ul");
      while (i < lines.length && /^[-*]\s+/.test(lines[i])) {
        const li = document.createElement("li");
        appendInlineText(li, lines[i].replace(/^[-*]\s+/, ""));
        ul.append(li);
        i++;
      }
      container.append(ul);
      continue;
    }

    if (/^\d+\.\s+/.test(line)) {
      flushParagraph();
      const ol = document.createElement("ol");
      while (i < lines.length && /^\d+\.\s+/.test(lines[i])) {
        const li = document.createElement("li");
        appendInlineText(li, lines[i].replace(/^\d+\.\s+/, ""));
        ol.append(li);
        i++;
      }
      container.append(ol);
      continue;
    }

    paragraphLines.push(line);
    i++;
  }
  flushParagraph();
}

function appendInlineText(parent, text) {
  // Longer/more specific patterns first so e.g. **bold** isn't split into
  // two stray *italic* matches by the shorter single-star alternative.
  const pieces = text.split(/(`[^`]+`|\*\*[^*]+\*\*|\*[^*]+\*|__[^_]+__|_[^_]+_)/g);
  for (const piece of pieces) {
    if (piece.startsWith("`") && piece.endsWith("`")) {
      const code = document.createElement("code");
      code.textContent = piece.slice(1, -1);
      parent.append(code);
    } else if (piece.startsWith("**") && piece.endsWith("**")) {
      const strong = document.createElement("strong");
      strong.textContent = piece.slice(2, -2);
      parent.append(strong);
    } else if (piece.startsWith("__") && piece.endsWith("__")) {
      const strong = document.createElement("strong");
      strong.textContent = piece.slice(2, -2);
      parent.append(strong);
    } else if (piece.startsWith("*") && piece.endsWith("*")) {
      const em = document.createElement("em");
      em.textContent = piece.slice(1, -1);
      parent.append(em);
    } else if (piece.startsWith("_") && piece.endsWith("_")) {
      const em = document.createElement("em");
      em.textContent = piece.slice(1, -1);
      parent.append(em);
    } else {
      parent.append(document.createTextNode(piece));
    }
  }
}

async function sendMessage(text) {
  text = text.trim();
  if (!text || activeController) return;
  const chat = activeChat() || createChat();
  const userMessage = { id: generateId(), role: "user", content: text };
  const assistantMessage = { id: generateId(), role: "assistant", content: "", streaming: true };
  chat.messages.push(userMessage, assistantMessage);
  if (chat.messages.length === 2) chat.title = makeTitle(text);
  chat.updatedAt = new Date().toISOString();
  saveState();
  render();
  setGenerating(true);

  activeController = new AbortController();
  const history = chat.messages
    .filter((message) => !message.streaming)
    .slice(-MAX_CONTEXT_MESSAGES)
    .map(({ role, content }) => ({ role, content }));
  const messages = [{ role: "system", content: state.preferences.system }, ...history];

  try {
    const response = await fetch("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        model: appConfig.model,
        messages,
        stream: true,
        temperature: Number(state.preferences.temperature),
      }),
      signal: activeController.signal,
    });
    if (!response.ok) throw new Error(await readAPIError(response));
    await consumeSSE(response, (chunk) => {
      assistantMessage.content += chunk;
      updateStreamingMessage(assistantMessage);
    });
    if (!assistantMessage.content) throw new Error("The model returned an empty response.");
  } catch (error) {
    if (error.name === "AbortError") {
      if (!assistantMessage.content) assistantMessage.content = "Response stopped.";
    } else {
      assistantMessage.content = `I couldn't complete that response. ${error.message}`;
    }
  } finally {
    assistantMessage.streaming = false;
    chat.updatedAt = new Date().toISOString();
    activeController = null;
    saveState();
    setGenerating(false);
    render();
  }
}

async function consumeSSE(response, onChunk) {
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  while (true) {
    const { value, done } = await reader.read();
    buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
    const events = buffer.split("\n\n");
    buffer = events.pop() || "";
    for (const event of events) {
      for (const line of event.split("\n")) {
        if (!line.startsWith("data:")) continue;
        const data = line.slice(5).trim();
        if (!data || data === "[DONE]") continue;
        const payload = JSON.parse(data);
        if (payload.error) throw new Error(payload.error.message || "Generation failed");
        const content = payload.choices?.[0]?.delta?.content;
        if (content) onChunk(content);
      }
    }
    if (done) break;
  }
}

function updateStreamingMessage(message) {
  const article = elements.messages.querySelector(`[data-message-id="${message.id}"]`);
  const content = article?.querySelector(".message-content");
  if (content) renderRichText(content, message.content);
  scrollToBottom();
}

async function readAPIError(response) {
  try {
    const body = await response.json();
    return body.error?.message || `Request failed (${response.status})`;
  } catch {
    return `Request failed (${response.status})`;
  }
}

function regenerate() {
  const chat = activeChat();
  if (!chat || activeController) return;
  while (chat.messages.at(-1)?.role === "assistant") chat.messages.pop();
  const lastUser = chat.messages.pop();
  if (lastUser?.role === "user") sendMessage(lastUser.content);
}

function setGenerating(generating) {
  elements.send.classList.toggle("hidden", generating);
  elements.stop.classList.toggle("hidden", !generating);
  elements.prompt.disabled = generating;
}

function makeTitle(text) {
  const words = text.replace(/\s+/g, " ").trim().split(" ").slice(0, 7).join(" ");
  return words.length < text.trim().length ? `${words}…` : words;
}

function resizePrompt() {
  elements.prompt.style.height = "auto";
  elements.prompt.style.height = `${Math.min(elements.prompt.scrollHeight, 180)}px`;
  elements.send.disabled = !elements.prompt.value.trim();
}

function scrollToBottom() {
  elements.conversation.scrollTop = elements.conversation.scrollHeight;
}

function openSidebar() {
  elements.sidebar.classList.add("open");
  elements.scrim.classList.add("open");
}

function closeSidebar() {
  elements.sidebar.classList.remove("open");
  elements.scrim.classList.remove("open");
}

function showToast(message) {
  clearTimeout(toastTimer);
  elements.toast.textContent = message;
  elements.toast.classList.add("show");
  toastTimer = setTimeout(() => elements.toast.classList.remove("show"), 1800);
}

async function initialize() {
  try {
    const response = await fetch("/api/config");
    if (response.ok) appConfig = await response.json();
  } catch { /* The UI still renders while the server starts. */ }
  elements.model.textContent = appConfig.model;
  document.querySelector(".model-state").textContent = appConfig.mock ? "Preview" : "Live";
  render();
  resizePrompt();
}

elements.form.addEventListener("submit", (event) => {
  event.preventDefault();
  const text = elements.prompt.value;
  elements.prompt.value = "";
  resizePrompt();
  sendMessage(text);
});
elements.prompt.addEventListener("input", resizePrompt);
elements.prompt.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
    event.preventDefault();
    elements.form.requestSubmit();
  }
});
elements.stop.addEventListener("click", () => activeController?.abort());
document.querySelector("#new-chat").addEventListener("click", createChat);
document.querySelector("#clear-chat").addEventListener("click", clearCurrentChat);
document.querySelector("#open-sidebar").addEventListener("click", openSidebar);
document.querySelector("#close-sidebar").addEventListener("click", closeSidebar);
elements.scrim.addEventListener("click", closeSidebar);
document.querySelectorAll(".suggestion").forEach((button) => button.addEventListener("click", () => sendMessage(button.dataset.prompt)));
document.addEventListener("keydown", (event) => {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    createChat();
  }
});
document.querySelector("#theme-button").addEventListener("click", () => {
  state.preferences.theme = state.preferences.theme === "dark" ? "light" : "dark";
  saveState();
  render();
});
document.querySelector("#settings-button").addEventListener("click", () => {
  elements.systemPrompt.value = state.preferences.system;
  elements.temperature.value = state.preferences.temperature;
  elements.temperatureValue.value = Number(state.preferences.temperature).toFixed(1);
  elements.dialog.showModal();
});
elements.temperature.addEventListener("input", () => {
  elements.temperatureValue.value = Number(elements.temperature.value).toFixed(1);
});
document.querySelector("#reset-settings").addEventListener("click", () => {
  elements.systemPrompt.value = DEFAULT_SYSTEM;
  elements.temperature.value = "0.7";
  elements.temperatureValue.value = "0.7";
});
document.querySelector("#save-settings").addEventListener("click", (event) => {
  event.preventDefault();
  state.preferences.system = elements.systemPrompt.value.trim() || DEFAULT_SYSTEM;
  state.preferences.temperature = Number(elements.temperature.value);
  saveState();
  elements.dialog.close();
  showToast("Preferences saved");
});

initialize();
