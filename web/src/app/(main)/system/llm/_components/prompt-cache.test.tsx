import "@testing-library/jest-dom/vitest";

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { NextIntlClientProvider } from "next-intl";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { api } from "@/lib/api";
import type { LLMProfile } from "@/lib/types";

import enMessages from "../../../../../../messages/en.json";
import { PromptCacheSection } from "./prompt-cache";

vi.mock("@/lib/api", () => ({
  api: {
    resetLLMProfileCache: vi.fn(),
  },
}));

beforeEach(() => {
  vi.clearAllMocks();
});
afterEach(cleanup);

function renderSection(profile: LLMProfile, onResetStats = vi.fn()) {
  return render(
    <NextIntlClientProvider locale="en" messages={enMessages}>
      <PromptCacheSection
        profile={profile}
        inputRate=""
        cachedRate=""
        outputRate=""
        onInputRateChange={() => {}}
        onCachedRateChange={() => {}}
        onOutputRateChange={() => {}}
        onResetStats={onResetStats}
      />
    </NextIntlClientProvider>,
  );
}

it("renders observed hit stats, unverified billing discount notice, and triggers reset", async () => {
  vi.mocked(api.resetLLMProfileCache).mockResolvedValue({
    prompt_cache: {
      profile_id: 12,
      status: "unknown",
      strategy: "automatic",
      hits: 0,
      misses: 0,
      metrics_unavailable_count: 0,
      requests_with_usage: 0,
      cached_input_tokens: 0,
      uncached_input_tokens: 0,
      cache_write_tokens: 0,
      output_tokens: 0,
      last_request_input_tokens: 0,
      last_request_cached_tokens: 0,
      last_request_uncached_tokens: 0,
    },
  });

  const profile: LLMProfile = {
    id: "12",
    name: "glm-5.3",
    format: "openai",
    model: "glm-5.3",
    rate_per_second: 0,
    rate_per_minute: 0,
    is_default: true,
    prompt_cache: {
      profile_id: 12,
      status: "observed_hit",
      strategy: "automatic",
      hits: 1,
      misses: 0,
      metrics_unavailable_count: 1,
      requests_with_usage: 2,
      cached_input_tokens: 2560,
      uncached_input_tokens: 2706,
      cache_write_tokens: 0,
      output_tokens: 4,
      last_request_input_tokens: 2633,
      last_request_cached_tokens: 2560,
      last_request_uncached_tokens: 73,
    },
    prompt_cache_hit_ratio: 2560 / (2560 + 2706),
    prompt_cache_cost: {
      rates_configured: false,
      discount_status: "unknown",
    },
  };

  const onResetStats = vi.fn();
  renderSection(profile, onResetStats);

  expect(screen.getByText("Cache Hit")).toBeInTheDocument();
  expect(screen.getByText("Unknown (not verified)")).toBeInTheDocument();
  expect(screen.getByText("2,560")).toBeInTheDocument();
  expect(screen.getByText("2,706")).toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "Reset stats" }));
  await waitFor(() => expect(api.resetLLMProfileCache).toHaveBeenCalledWith("12"));
  await waitFor(() => expect(onResetStats).toHaveBeenCalled());
});
