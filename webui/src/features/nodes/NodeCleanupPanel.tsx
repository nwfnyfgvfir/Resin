import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Eraser, Save } from "lucide-react";
import { useState } from "react";
import { Button } from "../../components/ui/Button";
import { Card } from "../../components/ui/Card";
import { Input } from "../../components/ui/Input";
import { Switch } from "../../components/ui/Switch";
import { useI18n } from "../../i18n";
import { formatApiErrorMessage } from "../../lib/error-message";
import { getSystemConfig, patchSystemConfig } from "../systemConfig/api";
import { cleanupNodes } from "./api";
import type { NodeCleanupMode, NodeCleanupResult } from "./types";

type Props = {
  selectedHashes: Set<string>;
  onCleaned: (result: NodeCleanupResult) => void;
  onClearSelection: () => void;
  showToast: (type: "success" | "error", message: string) => void;
};

export function NodeCleanupPanel({ selectedHashes, onCleaned, onClearSelection, showToast }: Props) {
  const { t } = useI18n();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState<{ enabled: boolean; threshold: string } | null>(null);
  const configQuery = useQuery({
    queryKey: ["system-config"],
    queryFn: getSystemConfig,
    staleTime: 30_000,
  });
  const saved = {
    enabled: configQuery.data?.auto_remove_high_latency_nodes_enabled ?? false,
    threshold: String(configQuery.data?.auto_remove_high_latency_nodes_threshold_ms ?? 1000),
  };
  const form = draft ?? saved;
  const threshold = Number(form.threshold);
  const validThreshold = form.threshold.trim() !== "" && Number.isInteger(threshold) && threshold > 0 && threshold <= 2147483647;
  const changed = form.enabled !== saved.enabled || form.threshold !== saved.threshold;

  const cleanupMutation = useMutation({
    mutationFn: ({ hashes, mode }: { hashes: string[]; mode: NodeCleanupMode }) => cleanupNodes(hashes, mode, mode === "high_latency" ? threshold : 0),
    onSuccess: async (result) => {
      onCleaned(result);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["nodes"] }),
        queryClient.invalidateQueries({ queryKey: ["node"] }),
        queryClient.invalidateQueries({ queryKey: ["subscriptions"] }),
        queryClient.invalidateQueries({ queryKey: ["platforms"] }),
        queryClient.invalidateQueries({ queryKey: ["dashboard"] }),
      ]);
      showToast("success", t("已清理 {{cleaned}} 个节点，跳过 {{skipped}} 个未满足条件的节点", {
        cleaned: result.cleaned_count, skipped: result.skipped_count,
      }));
    },
    onError: (error) => showToast("error", formatApiErrorMessage(error, t)),
  });

  const saveMutation = useMutation({
    mutationFn: () => patchSystemConfig({
      auto_remove_high_latency_nodes_enabled: form.enabled,
      auto_remove_high_latency_nodes_threshold_ms: threshold,
    }),
    onSuccess: (updated) => {
      queryClient.setQueryData(["system-config"], updated);
      setDraft(null);
      showToast("success", t("高延迟自动清理设置已保存"));
    },
    onError: (error) => showToast("error", formatApiErrorMessage(error, t)),
  });

  const runCleanup = (mode: NodeCleanupMode) => {
    if (!selectedHashes.size || cleanupMutation.isPending) return;
    cleanupMutation.mutate({ hashes: Array.from(selectedHashes), mode });
  };

  return (
    <Card className="node-cleanup-card">
      <div className="node-cleanup-heading">
        <div>
          <h3><Eraser size={16} />{t("节点清理")}</h3>
          <p>{t("仅清理选中节点。失效清理保留待测节点，高延迟清理保留无测速数据的节点。")}</p>
        </div>
        <div className="node-cleanup-selection" aria-live="polite">
          <span>{t("已选 {{count}} 个节点", { count: selectedHashes.size })}</span>
          {selectedHashes.size > 0 && (
            <Button size="sm" variant="ghost" onClick={onClearSelection} disabled={cleanupMutation.isPending}>{t("取消选择")}</Button>
          )}
        </div>
      </div>
      <div className="node-cleanup-controls">
        <div className="field-group node-cleanup-threshold">
          <label className="field-label" htmlFor="node-cleanup-threshold">{t("高延迟阈值（ms）")}</label>
          <Input
            id="node-cleanup-threshold"
            type="number"
            min={1}
            max={2147483647}
            step={1}
            value={form.threshold}
            invalid={!validThreshold}
            disabled={cleanupMutation.isPending || saveMutation.isPending}
            onChange={(event) => setDraft({ ...form, threshold: event.target.value })}
          />
        </div>
        <div className="node-cleanup-actions">
          <Button size="sm" variant="secondary" disabled={!selectedHashes.size || cleanupMutation.isPending}
            onClick={() => runCleanup("unhealthy")}>
            {cleanupMutation.isPending && cleanupMutation.variables?.mode === "unhealthy" ? t("清理中...") : t("清理选中失效节点")}
          </Button>
          <Button size="sm" variant="secondary" disabled={!selectedHashes.size || !validThreshold || cleanupMutation.isPending}
            onClick={() => runCleanup("high_latency")}>
            {cleanupMutation.isPending && cleanupMutation.variables?.mode === "high_latency" ? t("清理中...") : t("清理选中高延迟节点")}
          </Button>
        </div>
        <div className="node-cleanup-auto">
          <label htmlFor="node-auto-cleanup">{t("自动清理高延迟节点")}</label>
          <Switch id="node-auto-cleanup" checked={form.enabled}
            disabled={!configQuery.data || saveMutation.isPending}
            onChange={(event) => setDraft({ ...form, enabled: event.target.checked })} />
          <Button size="sm" variant="secondary" disabled={!configQuery.data || !changed || !validThreshold || saveMutation.isPending}
            onClick={() => saveMutation.mutate()}>
            <Save size={14} />{saveMutation.isPending ? t("保存中...") : t("保存设置")}
          </Button>
        </div>
      </div>
      <p className="field-hint">{t("超过阈值才会清理。自动清理对所有订阅生效，由服务端持续执行，关闭页面后仍生效。")}</p>
      {!validThreshold && <p className="field-error" role="alert">{t("延迟阈值必须是 1 到 2147483647 之间的整数")}</p>}
      {configQuery.isError && <p className="field-error" role="alert">{formatApiErrorMessage(configQuery.error, t)}</p>}
    </Card>
  );
}
