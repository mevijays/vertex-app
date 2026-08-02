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
  loginScreen: document.querySelector("#login-screen"),
  appShell: document.querySelector("#app-shell"),
  loginForm: document.querySelector("#login-form"),
  loginToken: document.querySelector("#login-token"),
  loginError: document.querySelector("#login-error"),
  attachmentsStrip: document.querySelector("#attachments-strip"),
  fileInput: document.querySelector("#file-input"),
  promptsDialog: document.querySelector("#prompts-dialog"),
  promptsList: document.querySelector("#prompts-list"),
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
  chat.conversationId = null;
  chat.documents = [];
  chat.updatedAt = new Date().toISOString();
  saveState();
  render();
  showToast("Conversation cleared");
}

function render() {
  renderHistory();
  renderMessages();
  renderAttachments();
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

  // Once a conversation_id exists (minted on first document attach), the
  // server already holds every prior turn and prepends it server-side —
  // sending the full local history too would duplicate it. Only the new
  // turn goes out; the system message is always sent since the server
  // extracts it separately and never persists or merges it.
  const userTurns = chat.conversationId
    ? [{ role: "user", content: text }]
    : chat.messages
        .filter((message) => !message.streaming)
        .slice(-MAX_CONTEXT_MESSAGES)
        .map(({ role, content }) => ({ role, content }));
  const messages = [{ role: "system", content: state.preferences.system }, ...userTurns];

  const body = {
    model: appConfig.model,
    messages,
    stream: true,
    temperature: Number(state.preferences.temperature),
  };
  if (chat.conversationId) body.conversation_id = chat.conversationId;

  try {
    const response = await fetch("/v1/chat/completions", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
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

// checkAuth doubles as the session bootstrap call: GET /api/config is
// gated the same as every other /api/* route, so a 401 here means "show
// the login screen" and a 200 means the vc_session cookie is now set too.
async function checkAuth() {
  try {
    const response = await fetch("/api/config");
    if (response.status === 401) return false;
    if (response.ok) appConfig = await response.json();
    return true;
  } catch {
    // The server may still be starting; treat as authenticated so the UI
    // renders instead of getting stuck behind a login screen it can't
    // reach anyway.
    return true;
  }
}

async function ensureConversationId(chat) {
  if (chat.conversationId) return chat.conversationId;
  const response = await fetch("/api/conversations", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title: chat.title === "New conversation" ? "" : chat.title }),
  });
  if (!response.ok) throw new Error(await readAPIError(response));
  const data = await response.json();
  chat.conversationId = data.id;
  saveState();
  return chat.conversationId;
}

async function handleFileSelected(event) {
  const file = event.target.files?.[0];
  event.target.value = "";
  if (!file) return;
  const chat = activeChat() || createChat();

  let conversationId;
  try {
    conversationId = await ensureConversationId(chat);
  } catch (error) {
    showToast(`Could not start attachment: ${error.message}`);
    return;
  }

  const placeholder = { id: generateId(), filename: file.name, status: "processing" };
  chat.documents = chat.documents || [];
  chat.documents.push(placeholder);
  saveState();
  renderAttachments();

  const formData = new FormData();
  formData.append("file", file);
  try {
    const response = await fetch(`/api/conversations/${conversationId}/documents`, {
      method: "POST",
      body: formData,
    });
    const data = await response.json().catch(() => null);
    if (!response.ok) throw new Error(data?.error?.message || `Upload failed (${response.status})`);
    Object.assign(placeholder, { id: data.id, status: data.status, error: data.error || "" });
    showToast(data.status === "ready" ? `${file.name} is ready` : `${file.name} failed: ${data.error || "analysis failed"}`);
  } catch (error) {
    placeholder.status = "failed";
    placeholder.error = error.message;
    showToast(`Upload failed: ${error.message}`);
  } finally {
    saveState();
    renderAttachments();
  }
}

function renderAttachments() {
  const chat = activeChat();
  const docs = chat?.documents || [];
  elements.attachmentsStrip.replaceChildren();
  for (const doc of docs) {
    const chip = document.createElement("span");
    chip.className = `attachment-chip ${doc.status}`;

    const icon = document.createElement("i");
    icon.className =
      doc.status === "ready" ? "fa fa-check-circle-o" : doc.status === "failed" ? "fa fa-warning" : "fa fa-spinner fa-spin";
    chip.append(icon, document.createTextNode(` ${doc.filename}`));

    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "attachment-remove";
    remove.setAttribute("aria-label", `Remove ${doc.filename}`);
    remove.innerHTML = '<i class="fa fa-times"></i>';
    remove.addEventListener("click", () => removeAttachment(chat, doc));
    chip.append(remove);

    elements.attachmentsStrip.append(chip);
  }
}

async function removeAttachment(chat, doc) {
  chat.documents = (chat.documents || []).filter((d) => d.id !== doc.id);
  saveState();
  renderAttachments();
  if (chat.conversationId && doc.status !== "processing") {
    try {
      await fetch(`/api/conversations/${chat.conversationId}/documents/${doc.id}`, { method: "DELETE" });
    } catch {
      /* best effort — the local chip is already gone */
    }
  }
}

async function loadPrompts() {
  try {
    const response = await fetch("/api/prompts");
    if (!response.ok) return [];
    const data = await response.json();
    return data.prompts || [];
  } catch {
    return [];
  }
}

async function renderPromptsList() {
  const prompts = await loadPrompts();
  elements.promptsList.replaceChildren();
  if (!prompts.length) {
    const empty = document.createElement("p");
    empty.className = "prompts-empty";
    empty.textContent = "No saved prompts yet.";
    elements.promptsList.append(empty);
    return;
  }
  for (const prompt of prompts) {
    const item = document.createElement("div");
    item.className = "prompt-item";

    const body = document.createElement("button");
    body.type = "button";
    body.className = "prompt-item-body";
    const title = document.createElement("div");
    title.className = "prompt-item-title";
    title.textContent = prompt.title;
    const content = document.createElement("div");
    content.className = "prompt-item-content";
    content.textContent = prompt.content;
    body.append(title, content);
    body.addEventListener("click", () => {
      elements.prompt.value = prompt.content;
      resizePrompt();
      elements.promptsDialog.close();
      elements.prompt.focus();
    });

    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "prompt-item-delete";
    remove.setAttribute("aria-label", `Delete ${prompt.title}`);
    remove.innerHTML = '<i class="fa fa-trash-o"></i>';
    remove.addEventListener("click", async (event) => {
      event.stopPropagation();
      await fetch(`/api/prompts/${prompt.id}`, { method: "DELETE" });
      renderPromptsList();
    });

    item.append(body, remove);
    elements.promptsList.append(item);
  }
}

async function saveCurrentPromptText(text, title) {
  const content = text.trim();
  if (!content) {
    showToast("Nothing to save yet");
    return;
  }
  await fetch("/api/prompts", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title: title || "", content }),
  });
  showToast("Prompt saved");
}

async function initialize() {
  const authed = await checkAuth();
  if (!authed) {
    elements.loginScreen.classList.remove("hidden");
    elements.appShell.classList.add("hidden");
    elements.loginToken.focus();
    return;
  }
  elements.loginScreen.classList.add("hidden");
  elements.appShell.classList.remove("hidden");
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

elements.loginForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  elements.loginError.classList.remove("show");
  const token = elements.loginToken.value;
  if (!token) return;
  try {
    const response = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ token }),
    });
    if (!response.ok) {
      elements.loginError.textContent = "Invalid token. Please try again.";
      elements.loginError.classList.add("show");
      elements.loginToken.select();
      return;
    }
    window.location.reload();
  } catch {
    elements.loginError.textContent = "Could not reach the server. Please try again.";
    elements.loginError.classList.add("show");
  }
});

document.querySelector("#attach-button").addEventListener("click", () => elements.fileInput.click());
elements.fileInput.addEventListener("change", handleFileSelected);

document.querySelector("#prompts-button").addEventListener("click", () => {
  elements.promptsDialog.showModal();
  renderPromptsList();
});
document.querySelector("#close-prompts").addEventListener("click", () => elements.promptsDialog.close());
document.querySelector("#save-prompt-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const titleInput = document.querySelector("#new-prompt-title");
  const contentInput = document.querySelector("#new-prompt-content");
  const content = contentInput.value.trim();
  if (!content) return;
  await saveCurrentPromptText(content, titleInput.value.trim());
  titleInput.value = "";
  contentInput.value = "";
  renderPromptsList();
});
document.querySelector("#save-prompt-button").addEventListener("click", () => {
  saveCurrentPromptText(elements.prompt.value, "");
});

initialize();
