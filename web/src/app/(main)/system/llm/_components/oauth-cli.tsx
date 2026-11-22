"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { ExternalCLIEvent, LLMProfile, OAuthCLIKind, OAuthCLIStatus } from "@/lib/types";

export function OAuthCLIProfile({
  profile,
  kind,
  onKindChange,
  onSaved,
  onClose,
}: {
  profile: LLMProfile | null;
  kind: OAuthCLIKind;
  onKindChange: (kind: LLMProfile["format"]) => void;
  onSaved: (id: string) => void;
  onClose: () => void;
}) {
  const [excluded, setExcluded] = React.useState(profile?.pool_exclude ?? false);
  const [name, setName] = React.useState(profile?.name ?? "");
  const [model, setModel] = React.useState(profile?.model ?? "");
  const [effort, setEffort] = React.useState(profile?.reasoning_effort || "default");
  const [systemInstructions, setSystemInstructions] = React.useState(profile?.system_instructions ?? "");
  const [status, setStatus] = React.useState<OAuthCLIStatus | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [running, setRunning] = React.useState(false);
  const [message, setMessage] = React.useState("");
  const [output, setOutput] = React.useState("");
  const [usage, setUsage] = React.useState("");
  const [task, setTask] = React.useState("");
  const [session, setSession] = React.useState("");
  const [prompt, setPrompt] = React.useState("");
  const controller = React.useRef<AbortController | null>(null);
  React.useEffect(() => {
    setSession(localStorage.getItem(`external-session-${profile?.id ?? kind}`) ?? crypto.randomUUID());
    setStatus(null);
    let active = true;
    api
      .oauthCLIStatus(kind)
      .then((s) => {
        if (active) setStatus(s);
      })
      .catch((e) => {
        if (active) setMessage((e as Error).message);
      });
    return () => {
      active = false;
      controller.current?.abort();
    };
  }, [profile?.id, kind]);
  function updateSession(value: string) {
    setSession(value);
    localStorage.setItem(`external-session-${profile?.id ?? kind}`, value);
  }
  async function auth(action: "connect" | "status" | "disconnect") {
    setBusy(true);
    setMessage("");
    try {
      if (action === "disconnect") {
        controller.current?.abort();
        await api.disconnectOAuthCLI(kind);
        setStatus(await api.oauthCLIStatus(kind));
      } else setStatus(await (action === "connect" ? api.connectOAuthCLI(kind) : api.oauthCLIStatus(kind)));
    } catch (e) {
      setMessage((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function save() {
    setBusy(true);
    setMessage("");
    try {
      const result = await api.saveLLMProfile({
        id: profile ? Number(profile.id) : undefined,
        name,
        model,
        reasoning_effort: effort === "default" ? "" : effort,
        format: kind,
        pool_exclude: kind === "agy-oauth" || excluded,
        streaming: true,
        system_instructions: systemInstructions,
      });
      onSaved(String(result.id));
      onClose();
    } catch (e) {
      setMessage((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function run(check = false) {
    if (!profile) return;
    const abort = new AbortController();
    controller.current = abort;
    setRunning(true);
    setMessage("starting");
    setOutput("");
    setUsage("");
    const emit = (event: ExternalCLIEvent) => {
      if (event.type === "text_delta") setOutput((text) => (text + (event.text ?? "")).slice(-65536));
      if (event.status) setMessage(event.status);
      if (event.usage) setUsage(JSON.stringify(event.usage));
    };
    try {
      if (check) {
        const result = await api.testLLM(
          kind,
          model,
          "",
          "",
          "",
          "",
          effort === "default" ? "" : effort,
          Number(profile.id),
        );
        if (!result.ok) throw new Error(result.error ?? "Connection failed");
        setOutput(result.reply ?? "");
        setMessage("connected");
        setStatus(await api.oauthCLIStatus(kind));
        return;
      }
      await api.streamExternalCLI(
        profile.id,
        {
          task_id: check ? "" : task,
          role: "external",
          session_id: check ? `connection-check-${crypto.randomUUID()}` : session,
          prompt: check ? "Reply only OK. Do not use tools or execute commands." : prompt,
        },
        abort.signal,
        emit,
      );
      setStatus(await api.oauthCLIStatus(kind));
    } catch (e) {
      setMessage(abort.signal.aborted ? "cancelled" : (e as Error).message);
    } finally {
      setRunning(false);
      controller.current = null;
    }
  }
  return (
    <>
      <SheetHeader>
        <SheetTitle>OAuth CLI</SheetTitle>
        <SheetDescription>
          {kind === "codex-oauth"
            ? "Codex supports ARTEX chat, Planner and Worker through native dynamic tools. ARTEX controls tool execution and permissions."
            : "Antigravity uses real MCP calls with ARTEX permissions. Gemini reads each prompt and agent instructions from a private session .txt file. Other AGY models keep fixed system instructions. Start a new chat when importing, editing or compacting history; pending calls cannot change tools."}{" "}
          Connect can reuse existing authorization; history stays separate. Disconnect leaves the original login intact.
        </SheetDescription>
      </SheetHeader>
      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
        <Label htmlFor="oauth-auth">Authentication</Label>
        <Select
          value={kind}
          disabled={!!profile || busy || running}
          onValueChange={(value) => onKindChange(value as LLMProfile["format"])}
        >
          <SelectTrigger id="oauth-auth">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="anthropic">API Key</SelectItem>
            <SelectItem value="codex-oauth">Codex CLI (ChatGPT OAuth)</SelectItem>
            <SelectItem value="agy-oauth">Antigravity CLI / agy (Google OAuth)</SelectItem>
          </SelectContent>
        </Select>
        <Label htmlFor="oauth-name">Profile name</Label>
        <Input id="oauth-name" value={name} onChange={(e) => setName(e.target.value)} />
        <p>Compatibility: {status?.compatibility ?? "checking"}</p>
        <p role="status">Authorization: {status?.state ?? "checking"}</p>
        <div className="flex flex-wrap gap-2">
          <Button disabled={busy || running} onClick={() => void auth("connect")}>
            Connect
          </Button>
          <Button
            variant="outline"
            disabled={busy || running}
            onClick={() => (profile ? void run(true) : void auth("status"))}
          >
            Check connection
          </Button>
          <Button variant="outline" disabled={busy} onClick={() => void auth("disconnect")}>
            Disconnect
          </Button>
        </div>
        {status?.instruction && (
          <div className="rounded border p-3 text-sm">
            <p>Terminal login / keyring instructions:</p>
            <code className="break-all whitespace-pre-wrap">{status.instruction}</code>
          </div>
        )}
        {status?.verification_url?.startsWith("https://") && (
          <p>
            Open{" "}
            <a className="underline" href={status.verification_url} target="_blank" rel="noreferrer">
              official login
            </a>{" "}
            and enter <strong>{status.user_code}</strong>. Then check connection.
          </p>
        )}
        <Label htmlFor="oauth-model">Model</Label>
        <Input
          id="oauth-model"
          list="oauth-models"
          value={model}
          onChange={(e) => setModel(e.target.value)}
          placeholder="Choose an available CLI model"
        />
        <datalist id="oauth-models">
          {status?.models.map((value) => (
            <option key={value} value={value} />
          ))}
        </datalist>
        <Label htmlFor="oauth-effort">Reasoning effort</Label>
        <Select value={effort} onValueChange={setEffort} disabled={busy || running}>
          <SelectTrigger id="oauth-effort" aria-describedby="oauth-effort-hint">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="default">Model default</SelectItem>
            {(kind === "agy-oauth"
              ? ["low", "medium", "high", "xhigh", "max"]
              : ["none", "minimal", "low", "medium", "high", "xhigh"]
            ).map((value) => (
              <SelectItem key={value} value={value}>
                {value}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <p id="oauth-effort-hint" className="text-muted-foreground text-xs">
          Overrides the model default for this profile. Supported levels depend on the selected model. Save to apply to
          Chat, Planner and Worker.
        </p>
        {kind === "codex-oauth" && (
          <label className="flex gap-2">
            <input type="checkbox" checked={excluded} onChange={(e) => setExcluded(e.target.checked)} />
            Exclude from rotation
          </label>
        )}
        <div className="grid gap-1">
          <Label htmlFor="oauth-sys-inst">System Instructions</Label>
          <Textarea
            id="oauth-sys-inst"
            rows={3}
            placeholder="Optional model-specific instructions (empty by default)"
            value={systemInstructions}
            onChange={(e) => setSystemInstructions(e.target.value)}
            disabled={busy || running}
          />
        </div>
        <Button disabled={busy || running || !name.trim() || !model.trim()} onClick={() => void save()}>
          Save profile
        </Button>
        {profile && (
          <>
            <Label>Assigned role</Label>
            <Input value={"Chat / Planner / Worker / Main Agent"} readOnly />
            <Label htmlFor="oauth-task">ARTEX task ID (optional, must be active)</Label>
            <Input id="oauth-task" value={task} onChange={(e) => setTask(e.target.value)} disabled={running} />
            <Label htmlFor="oauth-session">Session ID (reuse to resume)</Label>
            <div className="flex gap-2">
              <Input
                id="oauth-session"
                value={session}
                onChange={(e) => updateSession(e.target.value)}
                disabled={running}
              />
              <Button variant="outline" disabled={running} onClick={() => updateSession(crypto.randomUUID())}>
                New session
              </Button>
            </div>
            <Label htmlFor="oauth-prompt">Message</Label>
            <textarea
              id="oauth-prompt"
              className="min-h-24 rounded border p-2"
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
            />
            <div className="flex gap-2">
              <Button disabled={busy || running || !session || !prompt.trim()} onClick={() => void run()}>
                Run External Agent
              </Button>
              <Button variant="outline" disabled={!running} onClick={() => controller.current?.abort()}>
                Stop
              </Button>
            </div>
          </>
        )}
        <p aria-live="polite" className="text-sm whitespace-pre-wrap">
          {message}
        </p>
        {output && (
          <pre aria-live="polite" className="whitespace-pre-wrap break-words rounded border p-3 text-sm">
            {output}
          </pre>
        )}
        {usage && <p className="text-xs break-all">Usage: {usage}</p>}
      </div>
    </>
  );
}
