import "@testing-library/jest-dom/vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { Sheet, SheetContent } from "@/components/ui/sheet";
import { api } from "@/lib/api";
import type { LLMProfile, OAuthCLIKind } from "@/lib/types";

import { OAuthCLIProfile } from "./oauth-cli";

vi.mock("@/lib/api", () => ({
  api: {
    oauthCLIStatus: vi.fn(),
    saveLLMProfile: vi.fn(),
    testLLM: vi.fn(),
  },
}));

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.oauthCLIStatus).mockResolvedValue({ models: [], state: "connected" } as never);
  vi.mocked(api.saveLLMProfile).mockResolvedValue({ id: 7 });
  vi.mocked(api.testLLM).mockResolvedValue({ ok: true, reply: "OK" });
  Element.prototype.scrollIntoView = vi.fn();
});
afterEach(cleanup);

it.each<OAuthCLIKind>(["agy-oauth", "codex-oauth"])("loads, tests and saves effort for %s", async (kind) => {
  const profile: LLMProfile = {
    id: "7",
    name: "Profile",
    model: "model",
    format: kind,
    reasoning_effort: "high",
    rate_per_second: 0,
    rate_per_minute: 0,
    is_default: false,
  };
  render(
    <Sheet open>
      <SheetContent>
        <OAuthCLIProfile profile={profile} kind={kind} onKindChange={vi.fn()} onSaved={vi.fn()} onClose={vi.fn()} />
      </SheetContent>
    </Sheet>,
  );
  expect(screen.getByLabelText("Reasoning effort")).toHaveTextContent("high");
  fireEvent.click(screen.getByRole("button", { name: "Check connection" }));
  await waitFor(() => expect(api.testLLM).toHaveBeenCalledWith(kind, "model", "", "", "", "", "high", 7));
  await waitFor(() => expect(screen.getByRole("button", { name: "Save profile" })).toBeEnabled());
  fireEvent.click(screen.getByRole("button", { name: "Save profile" }));
  await waitFor(() =>
    expect(api.saveLLMProfile).toHaveBeenCalledWith(
      expect.objectContaining({ reasoning_effort: "high", format: kind }),
    ),
  );
  await waitFor(() => expect(screen.getByRole("button", { name: "Save profile" })).toBeEnabled());
  fireEvent.keyDown(screen.getByLabelText("Reasoning effort"), { key: "Enter" });
  fireEvent.click(await screen.findByRole("option", { name: "Model default" }));
  fireEvent.click(screen.getByRole("button", { name: "Save profile" }));
  await waitFor(() =>
    expect(api.saveLLMProfile).toHaveBeenLastCalledWith(expect.objectContaining({ reasoning_effort: "" })),
  );
});
