"use client";

import * as React from "react";

import { Loader2Icon, RotateCcwIcon } from "lucide-react";
import { useTranslations } from "next-intl";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import type { LLMProfile, PromptCacheStats, PromptCacheStatus } from "@/lib/types";
import { cn } from "@/lib/utils";

function formatNumber(n?: number | null): string {
  if (typeof n !== "number" || !Number.isFinite(n)) return "0";
  return n.toLocaleString();
}

function formatRatio(ratio?: number, totalTokens?: number): string {
  if (!totalTokens || totalTokens <= 0 || typeof ratio !== "number" || !Number.isFinite(ratio)) {
    return "—";
  }
  return `${(ratio * 100).toFixed(1)}%`;
}

function formatUSD(v?: number): string {
  if (typeof v !== "number" || !Number.isFinite(v)) return "—";
  if (v > 0 && v < 0.0001) return `$${v.toFixed(6)}`;
  return `$${v.toFixed(4)}`;
}

export function cacheStatusBadgeClass(status: PromptCacheStatus): string {
  switch (status) {
    case "observed_hit":
      return "border-emerald-500/50 text-emerald-600 dark:text-emerald-400";
    case "observed_miss":
      return "border-amber-500/50 text-amber-600 dark:text-amber-400";
    case "metrics_unavailable":
      return "border-slate-500/40 text-muted-foreground";
    case "native_unsupported":
      return "border-destructive/50 text-destructive";
    default:
      return "border-muted-foreground/30 text-muted-foreground";
  }
}

export function PromptCacheCardBadge({ profile }: { profile: LLMProfile }) {
  const t = useTranslations("llmPage");
  const pc = profile.prompt_cache;
  if (!pc || pc.status === "unknown") return null;

  const total = (pc.cached_input_tokens ?? 0) + (pc.uncached_input_tokens ?? 0);
  const ratioText =
    pc.status === "observed_hit" && total > 0 ? ` · ${formatRatio(profile.prompt_cache_hit_ratio, total)}` : "";
  const statusLabel = t(`promptCache.status.${pc.status}`);

  return (
    <Badge
      variant="outline"
      className={cn("font-normal text-[11px]", cacheStatusBadgeClass(pc.status))}
      title={t("promptCache.cardTooltip", {
        strategy: t(`promptCache.strategy.${pc.strategy || "automatic"}`),
        cached: formatNumber(pc.cached_input_tokens),
        uncached: formatNumber(pc.uncached_input_tokens),
      })}
    >
      {statusLabel}
      {ratioText}
    </Badge>
  );
}

export function PromptCacheSection({
  profile,
  inputRate,
  cachedRate,
  outputRate,
  onInputRateChange,
  onCachedRateChange,
  onOutputRateChange,
  onResetStats,
}: {
  profile: LLMProfile | null;
  inputRate: string;
  cachedRate: string;
  outputRate: string;
  onInputRateChange: (v: string) => void;
  onCachedRateChange: (v: string) => void;
  onOutputRateChange: (v: string) => void;
  onResetStats: (stats: PromptCacheStats) => void;
}) {
  const t = useTranslations("llmPage");
  const [resetting, setResetting] = React.useState(false);

  const pc = profile?.prompt_cache;
  const cost = profile?.prompt_cache_cost;
  const status: PromptCacheStatus = pc?.status ?? "unknown";
  const totalInput = (pc?.cached_input_tokens ?? 0) + (pc?.uncached_input_tokens ?? 0);

  async function handleReset() {
    if (!profile || resetting) return;
    setResetting(true);
    try {
      const { api } = await import("@/lib/api");
      const { prompt_cache } = await api.resetLLMProfileCache(profile.id);
      onResetStats(prompt_cache);
    } finally {
      setResetting(false);
    }
  }

  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="grid gap-0.5">
          <Label className="text-sm">{t("promptCache.title")}</Label>
          <p className="text-muted-foreground text-xs">{t("promptCache.subtitle")}</p>
        </div>
        {profile && (
          <div className="flex items-center gap-2">
            <Badge variant="outline" className={cacheStatusBadgeClass(status)}>
              {t(`promptCache.status.${status}`)}
            </Badge>
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={resetting}
              onClick={() => void handleReset()}
              title={t("promptCache.resetTitle")}
            >
              {resetting ? <Loader2Icon className="animate-spin" /> : <RotateCcwIcon />}
              {t("promptCache.reset")}
            </Button>
          </div>
        )}
      </div>

      {profile && pc && (
        <div className="grid gap-2 border-t pt-3 text-xs">
          <div className="grid grid-cols-2 gap-x-4 gap-y-1.5 sm:grid-cols-3">
            <div>
              <span className="block text-muted-foreground">{t("promptCache.strategyLabel")}</span>
              <span className="font-medium">{t(`promptCache.strategy.${pc.strategy || "automatic"}`)}</span>
            </div>
            <div>
              <span className="block text-muted-foreground">{t("promptCache.lastHitLabel")}</span>
              <span className="font-medium">
                {pc.last_cache_hit_at ? new Date(pc.last_cache_hit_at).toLocaleString() : t("promptCache.never")}
              </span>
            </div>
            <div>
              <span className="block text-muted-foreground">{t("promptCache.hitRatioLabel")}</span>
              <span className="font-medium font-mono">{formatRatio(profile.prompt_cache_hit_ratio, totalInput)}</span>
            </div>
            <div>
              <span className="block text-muted-foreground">{t("promptCache.cachedTokensLabel")}</span>
              <span className="font-medium font-mono">{formatNumber(pc.cached_input_tokens)}</span>
            </div>
            <div>
              <span className="block text-muted-foreground">{t("promptCache.uncachedTokensLabel")}</span>
              <span className="font-medium font-mono">{formatNumber(pc.uncached_input_tokens)}</span>
            </div>
            <div>
              <span className="block text-muted-foreground">{t("promptCache.requestsUsageLabel")}</span>
              <span className="font-medium font-mono">
                {formatNumber(pc.requests_with_usage)} ({pc.hits}H / {pc.misses}M / {pc.metrics_unavailable_count}U)
              </span>
            </div>
          </div>

          {pc.last_request_input_tokens > 0 && (
            <div className="rounded-md bg-muted/50 px-2.5 py-1.5 font-mono text-[11px] text-muted-foreground">
              {t("promptCache.lastRequestLine", {
                cached: formatNumber(pc.last_request_cached_tokens),
                uncached: formatNumber(pc.last_request_uncached_tokens),
                total: formatNumber(pc.last_request_input_tokens),
              })}
            </div>
          )}

          <div className="rounded-md border border-dashed px-2.5 py-2 text-muted-foreground">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="font-medium text-foreground">{t("promptCache.billingDiscountLabel")}</span>
              <span className="font-mono">
                {cost?.discount_status === "confirmed" && typeof cost.discount_percent === "number"
                  ? t("promptCache.discountConfirmed", {
                      pct: cost.discount_percent.toFixed(0),
                      savings: formatUSD(cost.savings_usd),
                    })
                  : cost?.discount_status === "none"
                    ? t("promptCache.discountNone")
                    : t("promptCache.discountUnknown")}
              </span>
            </div>
            {cost?.rates_configured && (
              <div className="mt-1 flex flex-wrap gap-3 font-mono text-[11px]">
                {typeof cost.total_cost_usd === "number" && (
                  <span>{t("promptCache.totalCost", { amount: formatUSD(cost.total_cost_usd) })}</span>
                )}
                {typeof cost.max_without_discount_usd === "number" && cost.discount_status === "unknown" && (
                  <span>
                    {t("promptCache.maxCostWithoutDiscount", { amount: formatUSD(cost.max_without_discount_usd) })}
                  </span>
                )}
              </div>
            )}
            <p className="mt-1 text-[11px]">{t("promptCache.discountDisclaimer")}</p>
          </div>
        </div>
      )}

      <div className="grid gap-2 border-t pt-3">
        <div className="grid gap-0.5">
          <Label className="text-xs">{t("promptCache.ratesTitle")}</Label>
          <p className="text-muted-foreground text-[11px]">{t("promptCache.ratesHint")}</p>
        </div>
        <div className="grid grid-cols-3 gap-2">
          <div className="grid gap-1">
            <Label htmlFor="p-rate-in" className="text-[11px]">
              {t("promptCache.rateInput")}
            </Label>
            <Input
              id="p-rate-in"
              type="number"
              min="0"
              step="0.0001"
              placeholder={t("promptCache.ratePlaceholder")}
              value={inputRate}
              onChange={(e) => onInputRateChange(e.target.value)}
            />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="p-rate-cached" className="text-[11px]">
              {t("promptCache.rateCached")}
            </Label>
            <Input
              id="p-rate-cached"
              type="number"
              min="0"
              step="0.0001"
              placeholder={t("promptCache.ratePlaceholder")}
              value={cachedRate}
              onChange={(e) => onCachedRateChange(e.target.value)}
            />
          </div>
          <div className="grid gap-1">
            <Label htmlFor="p-rate-out" className="text-[11px]">
              {t("promptCache.rateOutput")}
            </Label>
            <Input
              id="p-rate-out"
              type="number"
              min="0"
              step="0.0001"
              placeholder={t("promptCache.ratePlaceholder")}
              value={outputRate}
              onChange={(e) => onOutputRateChange(e.target.value)}
            />
          </div>
        </div>
      </div>
    </div>
  );
}
