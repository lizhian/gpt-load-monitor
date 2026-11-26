(function () {
  "use strict";

  const RANGE_KEYS = ["24h", "today", "yesterday", "7d", "30d"];
  const VALID_RANGES = new Set(RANGE_KEYS);
  const RANGE_LABELS = {
    "24h": "近 24 小时",
    today: "今天",
    yesterday: "昨天",
    "7d": "近 7 天",
    "30d": "近 30 天",
  };
  const DISPLAY_TIMEZONE = "Asia/Shanghai";
  const BEIJING_OFFSET_MS = 8 * 60 * 60 * 1000;
  const REFRESH_INTERVAL_MS = 60000;
  const RANKING_DEFAULT_LIMIT = 10;
  const ACCESS_KEY_PALETTE_SIZE = 12;

  function createRangeCache() {
    const cache = {};
    for (const key of RANGE_KEYS) {
      cache[key] = { data: null, updatedAtMs: null, error: null };
    }
    return cache;
  }

  const state = {
    range: "24h",
    accessKeySort: { column: 1, direction: "descending" },
    activeRankTab: "models",
    expandedRankTabs: {
      models: false,
      groups: false,
    },
    isLoading: false,
    requestSeq: 0,
    abortController: null,
    refreshTimerId: null,
    countdownTimerId: null,
    lastSyncMs: null,
    globalError: null,
    sections: {
      health: { data: null, updatedAtMs: null, error: null },
      concurrency: { data: null, updatedAtMs: null, error: null },
      quota: { data: null, updatedAtMs: null, error: null },
    },
    usageByRange: createRangeCache(),
    accessKeysByRange: createRangeCache(),
    chart: {
      series: [],
      range: "24h",
      granularity: "",
      hoveredIndex: -1,
      focusedIndex: -1,
      bars: [],
    },
    accessKeyCacheChart: {
      range: "24h",
      fromMs: null,
      toMs: null,
      buckets: [],
      series: [],
      bucketWidthMs: null,
      isDay: false,
      hiddenKeyIds: new Set(),
      colorByKeyId: new Map(),
      hoveredIndex: -1,
      focusedIndex: -1,
      interactionSource: null,
      pointsX: [],
      plotLeft: 0,
      plotRight: 0,
      legendSignature: "",
    },
  };

  /* ------------------------------------------------------------------------
     DOM Helpers (zero innerHTML; textContent & createElement only)
     ------------------------------------------------------------------------ */

  function byId(id) {
    return document.getElementById(id);
  }

  function clearChildren(el) {
    if (!el) return;
    while (el.firstChild) {
      el.removeChild(el.firstChild);
    }
  }

  function createEl(tag, options, children) {
    const el = document.createElement(tag);
    if (options) {
      if (options.className) el.className = options.className;
      if (options.text != null) el.textContent = String(options.text);
      if (options.attrs) {
        for (const [k, v] of Object.entries(options.attrs)) {
          if (v != null) el.setAttribute(k, String(v));
        }
      }
      if (options.dataset) {
        for (const [k, v] of Object.entries(options.dataset)) {
          if (v != null) el.dataset[k] = String(v);
        }
      }
    }
    if (children) {
      for (const child of children) {
        if (child == null) continue;
        if (typeof child === "string") {
          el.appendChild(document.createTextNode(child));
        } else {
          el.appendChild(child);
        }
      }
    }
    return el;
  }

  function setAlert(el, message) {
    if (!el) return;
    clearChildren(el);
    if (!message) {
      el.hidden = true;
      return;
    }
    el.appendChild(createEl("p", { className: "alert-text", text: message }));
    el.hidden = false;
  }

  /* ------------------------------------------------------------------------
     Formatters (Chinese locale, Beijing time Asia/Shanghai, defensive against null)
     ------------------------------------------------------------------------ */

  const BEIJING_PARTS_FORMATTER = (function () {
    try {
      if (typeof Intl !== "undefined" && typeof Intl.DateTimeFormat === "function") {
        return new Intl.DateTimeFormat("en-US", {
          timeZone: DISPLAY_TIMEZONE,
          year: "numeric",
          month: "2-digit",
          day: "2-digit",
          hour: "2-digit",
          minute: "2-digit",
          second: "2-digit",
          hourCycle: "h23",
        });
      }
    } catch (_err) {
      // fallback to fixed UTC+8 below
    }
    return null;
  })();

  function getBeijingParts(ms) {
    const n = toFiniteNum(ms);
    if (n === null) return null;
    const d = new Date(n);
    if (Number.isNaN(d.getTime())) return null;
    if (BEIJING_PARTS_FORMATTER) {
      try {
        const parts = BEIJING_PARTS_FORMATTER.formatToParts(d);
        const map = {};
        for (const p of parts) {
          if (p.type !== "literal") map[p.type] = p.value;
        }
        if (map.year && map.month && map.day && map.hour && map.minute && map.second) {
          return {
            year: map.year,
            month: map.month,
            day: map.day,
            hour: map.hour === "24" ? "00" : map.hour,
            minute: map.minute,
            second: map.second,
          };
        }
      } catch (_err) {
        // fallback below
      }
    }
    const shifted = new Date(n + BEIJING_OFFSET_MS);
    return {
      year: String(shifted.getUTCFullYear()),
      month: pad2(shifted.getUTCMonth() + 1),
      day: pad2(shifted.getUTCDate()),
      hour: pad2(shifted.getUTCHours()),
      minute: pad2(shifted.getUTCMinutes()),
      second: pad2(shifted.getUTCSeconds()),
    };
  }

  function isFiniteNum(v) {
    return typeof v === "number" && Number.isFinite(v);
  }

  function toFiniteNum(v) {
    if (typeof v === "number" && Number.isFinite(v)) return v;
    if (typeof v === "string" && v.trim() !== "") {
      const n = Number(v);
      if (Number.isFinite(n)) return n;
    }
    return null;
  }

  function formatInt(v, fallback) {
    const n = toFiniteNum(v);
    if (n === null) return fallback !== undefined ? fallback : "—";
    const rounded = Math.round(n);
    return (Object.is(rounded, -0) ? 0 : rounded).toLocaleString("zh-CN");
  }

  function formatCompactInt(v, fallback) {
    const n = toFiniteNum(v);
    if (n === null) return fallback !== undefined ? fallback : "—";
    const abs = Math.abs(n);
    if (abs === 0) return "0";
    if (abs >= 1e9) return (n / 1e9).toFixed(2) + "B";
    if (abs >= 1e6) return (n / 1e6).toFixed(2) + "M";
    if (abs >= 1e4) return (n / 1e3).toFixed(1) + "K";
    return Math.round(n).toLocaleString("zh-CN");
  }

  function formatAccessKeyTokens(value) {
    const tokens = toFiniteNum(value);
    if (tokens === null) return "暂无";
    if (tokens >= 1000) {
      const thousands = Math.round(tokens / 10) / 100;
      return thousands >= 1000 ? (tokens / 1e6).toFixed(2) + "M" : thousands + "K";
    }
    return formatInt(tokens);
  }

  function formatTokensPerRequest(v) {
    const n = toFiniteNum(v);
    if (n === null) return "暂无";
    const abs = Math.abs(n);
    if (abs === 0) return "0";
    if (abs >= 1e6) return (n / 1e6).toFixed(2) + "M";
    if (abs >= 1e4) return (n / 1e3).toFixed(1) + "K";
    if (Number.isInteger(n)) return n.toLocaleString("zh-CN");
    return n.toLocaleString("zh-CN", { minimumFractionDigits: 0, maximumFractionDigits: 1 });
  }

  function formatPercent(ratio, fallback) {
    const n = toFiniteNum(ratio);
    if (n === null) return fallback !== undefined ? fallback : "—";
    const pct = n * 100;
    if (pct === 0 || Object.is(pct, -0)) return "0%";
    if (pct === 100) return "100%";
    return pct.toFixed(1) + "%";
  }

  function formatSafeLevelPercent(percentValue) {
    const n = toFiniteNum(percentValue);
    if (n === null) return "暂无";
    const normalized = Object.is(n, -0) ? 0 : n;
    return normalized.toFixed(1) + "%";
  }

  function parseNanoUsd(raw) {
    if (raw === null || raw === undefined || raw === "") return null;
    if (typeof raw === "number") {
      return Number.isFinite(raw) ? raw / 1e9 : null;
    }
    const str = String(raw).trim();
    if (!/^-?\d+(\.\d+)?$/.test(str)) return null;
    if (/^-?\d+$/.test(str) && typeof BigInt === "function") {
      try {
        const neg = str.startsWith("-");
        const big = BigInt(neg ? str.slice(1) : str);
        const whole = big / 1000000000n;
        const frac = big % 1000000000n;
        const value = Number(whole) + Number(frac) / 1e9;
        return neg ? -value : value;
      } catch (_err) {
        // fallback below
      }
    }
    const n = Number(str);
    return Number.isFinite(n) ? n / 1e9 : null;
  }

  function formatUsdAmount(usd, fallback) {
    const n = toFiniteNum(usd);
    if (n === null) return fallback !== undefined ? fallback : "—";
    const abs = Math.abs(n);
    if (abs === 0) return "$0.00";
    const sign = n < 0 ? "-" : "";
    if (abs < 0.01) return sign + "$" + abs.toFixed(4);
    if (abs < 100) return sign + "$" + abs.toFixed(2);
    return sign + "$" + abs.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }

  function formatUsdFixed2(usd) {
    const n = toFiniteNum(usd);
    if (n === null) return "暂无";
    const rounded = Math.round(n * 100) / 100;
    const abs = Math.abs(rounded);
    if (abs === 0) return "$0.00";
    const sign = rounded < 0 ? "-" : "";
    return sign + "$" + abs.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }

  function formatUsdFromNano(raw, fallback) {
    const usd = parseNanoUsd(raw);
    return formatUsdAmount(usd, fallback);
  }

  function pad2(n) {
    return n < 10 ? "0" + n : String(n);
  }

  function formatLocalDateTime(ms) {
    const n = toFiniteNum(ms);
    if (n === null || n <= 0) return "—";
    const p = getBeijingParts(n);
    if (!p) return "—";
    return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}`;
  }

  function formatLocalShortTime(ms) {
    const n = toFiniteNum(ms);
    if (n === null || n <= 0) return "—";
    const p = getBeijingParts(n);
    const nowP = getBeijingParts(Date.now());
    if (!p) return "—";
    const sameDay = Boolean(
      nowP &&
      p.year === nowP.year &&
      p.month === nowP.month &&
      p.day === nowP.day
    );
    if (sameDay) {
      return `今日 ${p.hour}:${p.minute}`;
    }
    return `${p.month}-${p.day} ${p.hour}:${p.minute}`;
  }

  function formatGranularityLabel(granularity) {
    if (granularity == null) return "";
    const raw = String(granularity).trim();
    if (!raw) return "";
    const key = raw.toLowerCase();
    if (key === "minute" || key === "minutely" || key === "1m") return "按分钟";
    if (key === "hour" || key === "hourly" || key === "1h") return "按小时";
    if (key === "day" || key === "daily" || key === "1d") return "按天";
    const mMatch = key.match(/^(\d+)\s*(?:m|min|mins|minute|minutes)$/);
    if (mMatch) return mMatch[1] === "1" ? "按分钟" : `每 ${mMatch[1]} 分钟`;
    const hMatch = key.match(/^(\d+)\s*(?:h|hr|hrs|hour|hours)$/);
    if (hMatch) return hMatch[1] === "1" ? "按小时" : `每 ${hMatch[1]} 小时`;
    const dMatch = key.match(/^(\d+)\s*(?:d|day|days)$/);
    if (dMatch) return dMatch[1] === "1" ? "按天" : `每 ${dMatch[1]} 天`;
    return raw;
  }

  function isDayGranularity(range, granularity) {
    const key = granularity == null ? "" : String(granularity).trim().toLowerCase();
    if (key) {
      if (key === "day" || key === "daily" || /^\d+\s*(?:d|day|days)$/.test(key)) {
        return true;
      }
      if (
        key === "hour" ||
        key === "hourly" ||
        key === "minute" ||
        key === "minutely" ||
        /^\d+\s*(?:h|hr|hrs|hour|hours|m|min|mins|minute|minutes)$/.test(key)
      ) {
        return false;
      }
    }
    return range === "30d" || range === "7d";
  }

  function formatBucketAxisLabel(ms, range, granularity) {
    const p = getBeijingParts(ms);
    if (!p) return "";
    if (isDayGranularity(range, granularity)) {
      return `${p.month}-${p.day}`;
    }
    if (range === "7d" || range === "30d") {
      return `${p.month}-${p.day} ${p.hour}:${p.minute}`;
    }
    return `${p.hour}:${p.minute}`;
  }

  function formatBucketSpan(startMs, endMs, range, granularity) {
    const s = toFiniteNum(startMs);
    const e = toFiniteNum(endMs);
    if (s === null) return "—";
    const ps = getBeijingParts(s);
    if (!ps) return "—";
    if (isDayGranularity(range, granularity)) {
      const startStr = `${ps.month}-${ps.day}`;
      if (e !== null && e > s) {
        const pe = getBeijingParts(e - 1);
        if (pe) {
          const endStr = `${pe.month}-${pe.day}`;
          return startStr === endStr ? startStr : `${startStr} 至 ${endStr}`;
        }
      }
      return startStr;
    }
    const startStr = `${ps.month}-${ps.day} ${ps.hour}:${ps.minute}`;
    if (e !== null && e > s) {
      const pe = getBeijingParts(e);
      if (pe) {
        const peInclusive = getBeijingParts(e - 1);
        const sameDay = Boolean(
          peInclusive &&
          peInclusive.year === ps.year &&
          peInclusive.month === ps.month &&
          peInclusive.day === ps.day
        );
        const endStr = sameDay
          ? `${pe.hour}:${pe.minute}`
          : `${pe.month}-${pe.day} ${pe.hour}:${pe.minute}`;
        return `${startStr}–${endStr}`;
      }
    }
    return startStr;
  }

  function formatDurationSeconds(seconds) {
    const s = toFiniteNum(seconds);
    if (s === null || s < 0) return "—";
    const total = Math.floor(s);
    const days = Math.floor(total / 86400);
    const hours = Math.floor((total % 86400) / 3600);
    const mins = Math.floor((total % 3600) / 60);
    const secs = total % 60;
    if (days > 0) {
      return hours > 0 ? `${days} 天 ${hours} 小时` : `${days} 天`;
    }
    if (hours > 0) {
      return mins > 0 ? `${hours} 小时 ${mins} 分钟` : `${hours} 小时`;
    }
    if (mins > 0) {
      return `${mins} 分钟`;
    }
    return `${secs} 秒`;
  }

  function formatWindowDuration(seconds) {
    const s = toFiniteNum(seconds);
    if (s === null || s <= 0) return "";
    const total = Math.round(s);
    if (total % 86400 === 0) return `${total / 86400} 天`;
    if (total % 3600 === 0) return `${total / 3600} 小时`;
    if (total % 60 === 0) return `${total / 60} 分钟`;
    return `${total} 秒`;
  }

  function formatResetCountdown(resetAtMs, nowMs) {
    const target = toFiniteNum(resetAtMs);
    if (target === null || target <= 0) return "重置时间未知";
    const diffMs = target - nowMs;
    if (diffMs <= 0) {
      return `已到重置时间（${formatLocalShortTime(target)}）`;
    }
    const totalSec = Math.floor(diffMs / 1000);
    const days = Math.floor(totalSec / 86400);
    const hours = Math.floor((totalSec % 86400) / 3600);
    const mins = Math.floor((totalSec % 3600) / 60);
    const secs = totalSec % 60;
    let remaining;
    if (days > 0) {
      remaining = `${days} 天 ${hours} 小时`;
    } else if (hours > 0) {
      remaining = `${hours} 小时 ${mins} 分`;
    } else if (mins > 0) {
      remaining = `${mins} 分 ${secs} 秒`;
    } else {
      remaining = `${secs} 秒`;
    }
    return `${remaining}后重置（${formatLocalShortTime(target)}）`;
  }

  function rangeLabel(range) {
    return RANGE_LABELS[range] || RANGE_LABELS["24h"];
  }

  function mapStatusBadge(status) {
    const raw = status == null ? "" : String(status).trim();
    const key = raw.toLowerCase();
    if (!key) return { text: "未知状态", tone: "neutral" };
    if (key === "available" || key === "active" || key === "enabled" || key === "ok" || key === "healthy") {
      return { text: "可用", tone: "ok" };
    }
    if (key === "cooldown" || key === "model_cooldown" || key === "rate_limited" || key === "limited") {
      return { text: "冷却中", tone: "warn" };
    }
    if (key === "blacklisted" || key === "disabled" || key === "invalid" || key === "error" || key === "exhausted") {
      return { text: key === "blacklisted" ? "已拉黑" : key === "exhausted" ? "已耗尽" : "异常", tone: "bad" };
    }
    return { text: raw, tone: "neutral" };
  }

  function formatRawUnitValue(value, unit) {
    const n = toFiniteNum(value);
    if (n === null) return null;
    if (unit === "percent") {
      return (Number.isInteger(n) ? String(n) : n.toFixed(1)) + "%";
    }
    const formatted = Number.isInteger(n)
      ? n.toLocaleString("zh-CN")
      : n.toLocaleString("zh-CN", { maximumFractionDigits: 2 });
    return unit ? `${formatted} ${unit}` : formatted;
  }

  /* ------------------------------------------------------------------------
     URL Range Management
     ------------------------------------------------------------------------ */

  function readRangeFromUrl() {
    try {
      const params = new URLSearchParams(window.location.search);
      const r = params.get("range");
      return VALID_RANGES.has(r) ? r : "24h";
    } catch (_err) {
      return "24h";
    }
  }

  function syncRangeToUrl(range, push) {
    try {
      const url = new URL(window.location.href);
      url.searchParams.set("range", range);
      if (push) {
        window.history.pushState({ range }, "", url.toString());
      } else {
        window.history.replaceState({ range }, "", url.toString());
      }
    } catch (_err) {
      // ignore if History API unavailable
    }
  }

  function updateRangeControls() {
    const rangeButtons = document.querySelectorAll("[data-range]");
    for (const btn of rangeButtons) {
      btn.setAttribute("aria-pressed", String(btn.dataset.range === state.range));
    }
    const label = rangeLabel(state.range);
    const usageBadge = byId("usage-range-badge");
    const accessKeysBadge = byId("access-keys-range-badge");
    if (usageBadge) usageBadge.textContent = label;
    if (accessKeysBadge) accessKeysBadge.textContent = label;
  }

  /* ------------------------------------------------------------------------
     Section Renderers
     ------------------------------------------------------------------------ */

  function renderTopStatus() {
    const statusEl = byId("global-sync-status");
    const refreshBtn = byId("refresh-btn");
    const globalErrEl = byId("global-error-banner");

    if (refreshBtn) {
      refreshBtn.disabled = state.isLoading;
      refreshBtn.setAttribute("aria-busy", String(state.isLoading));
    }

    setAlert(globalErrEl, state.globalError);

    if (!statusEl) return;
    if (state.isLoading) {
      statusEl.textContent = state.lastSyncMs
        ? `正在刷新（上次同步 ${formatLocalDateTime(state.lastSyncMs)}）`
        : "正在加载监控数据…";
      return;
    }

    const activeUsage = state.usageByRange[state.range];
    const activeAccessKeys = state.accessKeysByRange[state.range];
    const hasAnyError = Boolean(
      state.globalError ||
      state.sections.health.error ||
      state.sections.concurrency.error ||
      state.sections.quota.error ||
      activeUsage.error ||
      activeAccessKeys.error
    );

    if (state.lastSyncMs) {
      statusEl.textContent = hasAnyError
        ? `最近同步 ${formatLocalDateTime(state.lastSyncMs)}，部分模块拉取失败`
        : `最近同步 ${formatLocalDateTime(state.lastSyncMs)}，每 60 秒自动刷新`;
    } else if (hasAnyError) {
      statusEl.textContent = "数据加载失败";
    } else {
      statusEl.textContent = "等待数据";
    }
  }

  function renderAccessKeysSection() {
    const activeRange = state.range;
    expireCalendarCache(activeRange);
    const akState = state.accessKeysByRange[activeRange];
    const sectionEl = byId("section-access-keys");
    const countBadgeEl = byId("access-keys-count-badge");
    const metaEl = byId("access-keys-meta");
    const caveatsEl = byId("access-keys-caveats");
    const emptyEl = byId("access-keys-empty");
    const scrollEl = byId("access-keys-scroll");
    const bodyEl = byId("access-keys-body");

    if (sectionEl) {
      sectionEl.setAttribute("aria-busy", String(state.isLoading && !akState.data));
    }

    updateRangeControls();

    if (metaEl) {
      const parts = [];
      if (akState.updatedAtMs) {
        parts.push(`更新 ${formatLocalDateTime(akState.updatedAtMs)}`);
      }
      if (akState.data && toFiniteNum(akState.data.observed_at_ms)) {
        parts.push(`上游观测 ${formatLocalDateTime(akState.data.observed_at_ms)}`);
      }
      metaEl.textContent = parts.length > 0 ? parts.join("  |  ") : (state.isLoading ? "加载中…" : "暂无更新时间");
    }

    setAlert(
      byId("access-keys-error"),
      akState.error
        ? akState.data
          ? `${rangeLabel(activeRange)} AccessKey 统计获取失败：${akState.error}（当前保留该范围上次成功数据）`
          : `${rangeLabel(activeRange)} AccessKey 统计获取失败：${akState.error}`
        : null
    );

    const data = akState.data;
    const items = data && Array.isArray(data.items) ? data.items : [];

    if (countBadgeEl) countBadgeEl.textContent = `${items.length} 项`;
    clearChildren(bodyEl);

    if (!data) {
      if (caveatsEl) caveatsEl.hidden = true;
      if (emptyEl) {
        emptyEl.textContent = akState.error ? "暂无可用 AccessKey 统计数据" : `正在加载${rangeLabel(activeRange)} AccessKey 统计…`;
        emptyEl.hidden = false;
      }
      if (scrollEl) scrollEl.hidden = true;
      renderAccessKeyCacheSection(activeRange, akState);
      return;
    }

    if (items.length === 0) {
      if (caveatsEl) caveatsEl.hidden = true;
      if (emptyEl) {
        emptyEl.textContent = "该时间范围内暂无 AccessKey 统计记录";
        emptyEl.hidden = false;
      }
      if (scrollEl) scrollEl.hidden = true;
      renderAccessKeyCacheSection(activeRange, akState);
      return;
    }

    if (emptyEl) emptyEl.hidden = true;
    if (scrollEl) scrollEl.hidden = false;

    let incompleteCount = 0;
    let rowErrorCount = 0;

    for (const item of items) {
      if (!item || typeof item !== "object") continue;

      const nameText = getAccessKeyDisplayName(item);
      const pricingIncomplete = Boolean(item.pricing_incomplete);
      const rowError = item.error ? String(item.error).trim() : "";

      if (pricingIncomplete) incompleteCount++;
      if (rowError) rowErrorCount++;

      const nameRowChildren = [
        createEl("span", {
          className: "cell-entity-name",
          text: nameText,
          attrs: { title: nameText },
        }),
      ];
      if (item.deleted) {
        nameRowChildren.push(createEl("span", { className: "badge badge--neutral", text: "已删除" }));
      }
      if (pricingIncomplete) {
        nameRowChildren.push(
          createEl("span", {
            className: "badge badge--warn",
            text: "计价不全",
            attrs: { title: "部分请求缺少模型定价，费用指标可能不完整" },
          })
        );
      }
      if (rowError) {
        nameRowChildren.push(
          createEl("span", {
            className: "badge badge--bad",
            text: item.stale ? "上次数据" : "统计异常",
            attrs: { title: rowError },
          })
        );
      }

      const identityChildren = [createEl("span", { className: "cell-entity-row" }, nameRowChildren)];
      if (rowError) {
        identityChildren.push(
          createEl("span", {
            className: "cell-entity-error",
            text: rowError,
            attrs: { title: rowError },
          })
        );
      }

      const totalCostText = formatUsdFromNano(item.estimated_cost_nano_usd, "暂无");
      const totalTokensText = formatAccessKeyTokens(item.total_tokens);
      const reqCountText = formatInt(item.request_count, "暂无");
      const cacheHitText = formatPercent(item.cache_hit_rate, "暂无");
      const costPerMText = formatUsdAmount(item.cost_per_million_tokens_usd, "暂无");
      const costPerKReqText = formatUsdAmount(item.cost_per_thousand_requests_usd, "暂无");
      const tokensPerReqText = formatTokensPerRequest(item.tokens_per_request);

      const metricCell = (text, value, title) =>
        createEl("td", {
          className: text === "暂无" ? "num text-muted" : "num",
          text,
          attrs: { "data-value": value, title },
        });

      const tr = createEl("tr", rowError ? { className: "row--error" } : null, [
        createEl("th", { attrs: { scope: "row" }, className: "col-sticky" }, [
          createEl("div", { className: "cell-entity" }, identityChildren),
        ]),
        metricCell(totalCostText, item.estimated_cost_nano_usd),
        metricCell(totalTokensText, item.total_tokens, formatInt(item.total_tokens, null)),
        metricCell(reqCountText, item.request_count),
        metricCell(cacheHitText, item.cache_hit_rate),
        metricCell(costPerMText, item.cost_per_million_tokens_usd),
        metricCell(costPerKReqText, item.cost_per_thousand_requests_usd),
        metricCell(tokensPerReqText, item.tokens_per_request),
      ]);
      bodyEl.appendChild(tr);
    }
    sortAccessKeyRows();

    if (caveatsEl) {
      const notes = [];
      if (incompleteCount > 0) {
        notes.push(`${incompleteCount} 个 AccessKey 存在未计价或部分计价请求，费用指标可能偏低`);
      }
      if (rowErrorCount > 0) {
        notes.push(`${rowErrorCount} 个 AccessKey 统计异常`);
      }
      if (notes.length > 0) {
        caveatsEl.textContent = `统计提示：${notes.join("；")}。`;
        caveatsEl.hidden = false;
      } else {
        caveatsEl.hidden = true;
      }
    }

    renderAccessKeyCacheSection(activeRange, akState);
  }

  function sortAccessKeyRows() {
    const body = byId("access-keys-body");
    const { column, direction } = state.accessKeySort;
    const factor = direction === "ascending" ? 1 : -1;
    const rows = Array.from(body.rows, row => {
      const raw = row.cells[column].dataset.value;
      // GPT-Load sends currency in whole nano-dollars as a string.
      const value = column === 1 && /^-?\d+$/.test(raw) ? BigInt(raw) : toFiniteNum(raw);
      return { row, value };
    });
    rows.sort((a, b) => {
      if (a.value === null) return b.value === null ? 0 : 1;
      if (b.value === null) return -1;
      return factor * (a.value > b.value ? 1 : a.value < b.value ? -1 : 0);
    });
    for (const { row } of rows) {
      if (typeof body.moveBefore === "function") body.moveBefore(row, null);
      else body.appendChild(row);
    }
  }

  function setAccessKeySort(button) {
    const header = button.closest("th");
    const column = header.cellIndex;
    const last = state.accessKeySort;
    const direction = last.column === column && last.direction === "descending" ? "ascending" : "descending";
    header.parentElement.cells[last.column].removeAttribute("aria-sort");
    header.setAttribute("aria-sort", direction);
    state.accessKeySort = { column, direction };
    sortAccessKeyRows();
    const count = byId("access-keys-body").rows.length;
    byId("access-key-sort-status").textContent =
      `按${button.textContent.trim()}${direction === "ascending" ? "升序" : "降序"}排列，共 ${count} 项。`;
  }

  function renderHealthSection() {
    const sectionEl = byId("section-health");
    const healthState = state.sections.health;
    const concState = state.sections.concurrency;

    if (sectionEl) {
      sectionEl.setAttribute("aria-busy", String(state.isLoading && !healthState.data && !concState.data));
    }

    // Reachability badge
    const badgeEl = byId("health-reachability");
    if (badgeEl) {
      if (healthState.error) {
        badgeEl.textContent = healthState.data ? "异常（缓存）" : "不可达";
        badgeEl.className = healthState.data ? "badge badge--warn" : "badge badge--bad";
      } else if (healthState.data) {
        badgeEl.textContent = "服务可达";
        badgeEl.className = "badge badge--ok";
      } else if (state.isLoading) {
        badgeEl.textContent = "检查中";
        badgeEl.className = "badge badge--neutral";
      } else {
        badgeEl.textContent = "未知";
        badgeEl.className = "badge badge--neutral";
      }
    }

    // Section timestamp meta
    const metaEl = byId("health-meta");
    if (metaEl) {
      const parts = [];
      if (healthState.updatedAtMs) {
        parts.push(`健康更新 ${formatLocalDateTime(healthState.updatedAtMs)}`);
      }
      if (healthState.data && toFiniteNum(healthState.data.observed_at_ms)) {
        parts.push(`上游观测 ${formatLocalDateTime(healthState.data.observed_at_ms)}`);
      }
      if (concState.updatedAtMs && concState.updatedAtMs !== healthState.updatedAtMs) {
        parts.push(`并发更新 ${formatLocalDateTime(concState.updatedAtMs)}`);
      }
      metaEl.textContent = parts.length > 0 ? parts.join("  |  ") : (state.isLoading ? "加载中…" : "暂无更新时间");
    }

    // Errors (preserve last successful data visible underneath)
    setAlert(
      byId("health-error"),
      healthState.error
        ? healthState.data
          ? `服务健康获取失败：${healthState.error}（当前保留上次成功数据）`
          : `服务健康获取失败：${healthState.error}`
        : null
    );
    setAlert(
      byId("concurrency-error"),
      concState.error
        ? concState.data
          ? `并发状态获取失败：${concState.error}（当前保留上次成功数据）`
          : `并发状态获取失败：${concState.error}`
        : null
    );

    // Health summary metrics
    const hData = healthState.data;
    const versionEl = byId("health-version");
    const uptimeEl = byId("health-uptime");
    const availEl = byId("health-available");
    const totalCredsEl = byId("health-total-creds");
    const degradedEl = byId("health-degraded");
    const degradedDetailEl = byId("health-degraded-detail");

    if (hData) {
      if (versionEl) versionEl.textContent = hData.version ? String(hData.version) : "未知版本";
      if (uptimeEl) uptimeEl.textContent = `运行时间 ${formatDurationSeconds(hData.uptime_seconds)}`;

      const counts = hData.counts || {};
      const total = toFiniteNum(counts.credentials);
      const available = toFiniteNum(counts.available);
      const cooldown = toFiniteNum(counts.cooldown);
      const blacklisted = toFiniteNum(counts.blacklisted);

      if (availEl) {
        availEl.textContent = total !== null && available !== null
          ? `${formatInt(available)} / ${formatInt(total)}`
          : formatInt(available);
      }
      if (totalCredsEl) {
        totalCredsEl.textContent = total !== null ? `总凭证 ${formatInt(total)}` : "总凭证 —";
      }

      const degradedSum = (cooldown || 0) + (blacklisted || 0);
      if (degradedEl) {
        degradedEl.textContent = cooldown === null && blacklisted === null ? "—" : formatInt(degradedSum);
      }
      if (degradedDetailEl) {
        degradedDetailEl.textContent = `冷却 ${formatInt(cooldown)} / 拉黑 ${formatInt(blacklisted)}`;
      }
    } else {
      if (versionEl) versionEl.textContent = "—";
      if (uptimeEl) uptimeEl.textContent = "运行时间 —";
      if (availEl) availEl.textContent = "—";
      if (totalCredsEl) totalCredsEl.textContent = "总凭证 —";
      if (degradedEl) degradedEl.textContent = "—";
      if (degradedDetailEl) degradedDetailEl.textContent = "冷却 — / 拉黑 —";
    }

    // Concurrency tile
    const cData = concState.data;
    const concReadingEl = byId("concurrency-reading");
    const concSubEl = byId("concurrency-sub");
    if (cData) {
      const current = toFiniteNum(cData.current);
      const limit = toFiniteNum(cData.limit);
      const unlimited = limit !== null && limit <= 0;
      if (concReadingEl) {
        concReadingEl.textContent = unlimited
          ? `${formatInt(current)} / 无限制`
          : `${formatInt(current)} / ${formatInt(limit)}`;
      }
      if (concSubEl) {
        if (unlimited) {
          concSubEl.textContent = "并发上限未限制";
        } else if (current !== null && limit !== null && limit > 0) {
          concSubEl.textContent = `占用率 ${formatPercent(current / limit)}`;
        } else {
          concSubEl.textContent = "实时并发计数";
        }
      }
    } else {
      if (concReadingEl) concReadingEl.textContent = "—";
      if (concSubEl) concSubEl.textContent = concState.error ? "并发暂不可用" : "等待并发数据";
    }

    // Groups table
    const groups = hData && Array.isArray(hData.groups) ? hData.groups : [];
    const groupsCountEl = byId("health-groups-count");
    const groupsEmptyEl = byId("health-groups-empty");
    const groupsScrollEl = byId("health-groups-scroll");
    const groupsBodyEl = byId("health-groups-body");

    if (groupsCountEl) groupsCountEl.textContent = `${groups.length} 个分组`;
    clearChildren(groupsBodyEl);

    if (!hData) {
      if (groupsEmptyEl) {
        groupsEmptyEl.textContent = healthState.error ? "暂无可用分组状态" : "正在加载分组状态…";
        groupsEmptyEl.hidden = false;
      }
      if (groupsScrollEl) groupsScrollEl.hidden = true;
      return;
    }

    if (groups.length === 0) {
      if (groupsEmptyEl) {
        groupsEmptyEl.textContent = "暂无分组数据";
        groupsEmptyEl.hidden = false;
      }
      if (groupsScrollEl) groupsScrollEl.hidden = true;
      return;
    }

    if (groupsEmptyEl) groupsEmptyEl.hidden = true;
    if (groupsScrollEl) groupsScrollEl.hidden = false;

    for (const grp of groups) {
      if (!grp || typeof grp !== "object") continue;
      const gCounts = grp.counts || {};
      const enabled = Boolean(grp.enabled);
      const nameText = grp.name ? String(grp.name) : (grp.id != null ? `分组 #${grp.id}` : "未命名分组");

      const tr = createEl("tr", null, [
        createEl("th", { attrs: { scope: "row" }, className: "col-sticky" }, [
          createEl("span", { className: "cell-entity" }, [
            createEl("span", { className: "cell-entity-name", text: nameText, attrs: { title: nameText } }),
          ]),
        ]),
        createEl("td", null, [
          createEl("span", {
            className: enabled ? "badge badge--ok" : "badge badge--neutral",
            text: enabled ? "启用" : "停用",
          }),
        ]),
        createEl("td", { className: "num", text: formatInt(gCounts.credentials) }),
        createEl("td", { className: "num text-ok", text: formatInt(gCounts.available) }),
        createEl("td", { className: "num" + (toFiniteNum(gCounts.cooldown) > 0 ? " text-warn" : ""), text: formatInt(gCounts.cooldown) }),
        createEl("td", { className: "num" + (toFiniteNum(gCounts.model_cooldown) > 0 ? " text-warn" : ""), text: formatInt(gCounts.model_cooldown) }),
        createEl("td", { className: "num" + (toFiniteNum(gCounts.blacklisted) > 0 ? " text-bad" : ""), text: formatInt(gCounts.blacklisted) }),
      ]);
      groupsBodyEl.appendChild(tr);
    }
  }

  function renderUsageSection() {
    expireCalendarCache(state.range);
    const activeRange = state.range;
    const usageState = state.usageByRange[activeRange];
    const sectionEl = byId("section-usage");

    if (sectionEl) {
      sectionEl.setAttribute("aria-busy", String(state.isLoading && !usageState.data));
    }

    updateRangeControls();

    const metaEl = byId("usage-meta");
    if (metaEl) {
      const parts = [];
      if (usageState.updatedAtMs) {
        parts.push(`更新 ${formatLocalDateTime(usageState.updatedAtMs)}`);
      }
      if (usageState.data && toFiniteNum(usageState.data.observed_at_ms)) {
        parts.push(`上游观测 ${formatLocalDateTime(usageState.data.observed_at_ms)}`);
      }
      metaEl.textContent = parts.length > 0 ? parts.join("  |  ") : (state.isLoading ? "加载中…" : "暂无更新时间");
    }

    setAlert(
      byId("usage-error"),
      usageState.error
        ? usageState.data
          ? `${rangeLabel(activeRange)}用量获取失败：${usageState.error}（当前保留该范围上次成功数据）`
          : `${rangeLabel(activeRange)}用量获取失败：${usageState.error}`
        : null
    );

    const data = usageState.data;
    const summary = data && data.summary && typeof data.summary === "object" ? data.summary : null;

    const reqEl = byId("usage-requests");
    const reqSubEl = byId("usage-requests-sub");
    const rateEl = byId("usage-success-rate");
    const rateSubEl = byId("usage-success-sub");
    const tokEl = byId("usage-total-tokens");
    const tokSubEl = byId("usage-tokens-sub");
    const costEl = byId("usage-cost");
    const costSubEl = byId("usage-cost-sub");
    const caveatsEl = byId("usage-caveats");
    const winLabelEl = byId("usage-window-label");

    if (summary) {
      const reqCount = toFiniteNum(summary.request_count);
      const okCount = toFiniteNum(summary.success_count);
      const failCount = toFiniteNum(summary.failure_count);

      if (reqEl) reqEl.textContent = formatInt(reqCount);
      if (reqSubEl) reqSubEl.textContent = `成功 ${formatInt(okCount)} / 失败 ${formatInt(failCount)}`;

      if (reqCount !== null && reqCount > 0 && okCount !== null) {
        const rate = Math.max(0, Math.min(1, okCount / reqCount));
        if (rateEl) rateEl.textContent = formatPercent(rate);
        if (rateSubEl) rateSubEl.textContent = failCount > 0 ? `失败请求 ${formatInt(failCount)} 次` : "无失败请求";
      } else {
        if (rateEl) rateEl.textContent = "暂无";
        if (rateSubEl) rateSubEl.textContent = "无请求记录，成功率不可用";
      }

      if (tokEl) tokEl.textContent = formatCompactInt(summary.total_tokens);
      if (tokSubEl) {
        tokSubEl.textContent = `输入 ${formatCompactInt(summary.input_tokens)} / 缓存读取 ${formatCompactInt(summary.cache_read_tokens)}`;
      }

      if (costEl) costEl.textContent = formatUsdFromNano(summary.estimated_cost_nano_usd);
      if (costSubEl) {
        const unpriced = toFiniteNum(summary.unpriced_request_count) || 0;
        const partial = toFiniteNum(summary.pricing_partial_count) || 0;
        costSubEl.textContent = unpriced > 0 || partial > 0
          ? "存在未计价或部分计价请求"
          : "按模型定价估算（USD）";
      }

      if (caveatsEl) {
        const caveats = [];
        const unpriced = toFiniteNum(summary.unpriced_request_count) || 0;
        const partial = toFiniteNum(summary.pricing_partial_count) || 0;
        const missing = toFiniteNum(summary.usage_missing_count) || 0;
        if (unpriced > 0) caveats.push(`${formatInt(unpriced)} 次未计价`);
        if (partial > 0) caveats.push(`${formatInt(partial)} 次部分计价`);
        if (missing > 0) caveats.push(`${formatInt(missing)} 次缺失用量`);
        if (caveats.length > 0) {
          caveatsEl.textContent = `计价与统计提示：包含 ${caveats.join("、")}。`;
          caveatsEl.hidden = false;
        } else {
          caveatsEl.hidden = true;
        }
      }
    } else {
      if (reqEl) reqEl.textContent = "—";
      if (reqSubEl) reqSubEl.textContent = "成功 — / 失败 —";
      if (rateEl) rateEl.textContent = "—";
      if (rateSubEl) rateSubEl.textContent = usageState.error ? "用量数据不可用" : "等待用量数据";
      if (tokEl) tokEl.textContent = "—";
      if (tokSubEl) tokSubEl.textContent = "输入 — / 缓存读取 —";
      if (costEl) costEl.textContent = "—";
      if (costSubEl) costSubEl.textContent = "美元估算值";
      if (caveatsEl) caveatsEl.hidden = true;
    }

    const granularity = data && data.granularity ? String(data.granularity) : "";

    if (winLabelEl) {
      if (data && toFiniteNum(data.from_ms) && toFiniteNum(data.to_ms)) {
        const granLabel = formatGranularityLabel(granularity);
        const gran = granLabel ? `（粒度：${granLabel}）` : "";
        winLabelEl.textContent = `${formatLocalShortTime(data.from_ms)} 至 ${formatLocalShortTime(data.to_ms)}${gran}`;
      } else {
        winLabelEl.textContent = rangeLabel(activeRange);
      }
    }

    // Series chart & bucket alternative table
    const series = data && Array.isArray(data.series) ? data.series : [];
    state.chart.series = series;
    state.chart.range = activeRange;
    state.chart.granularity = granularity;
    state.chart.hoveredIndex = -1;
    state.chart.focusedIndex = -1;
    renderTrendChart();
    renderBucketTable(series, activeRange, granularity);

    // Rankings
    const rankings = data && data.rankings && typeof data.rankings === "object" ? data.rankings : null;
    renderRankings(rankings, Boolean(data), Boolean(usageState.error));
  }

  function renderBucketTable(series, range, granularity) {
    const tbody = byId("usage-buckets-body");
    if (!tbody) return;
    clearChildren(tbody);

    if (!series || series.length === 0) {
      tbody.appendChild(
        createEl("tr", null, [
          createEl("td", { attrs: { colspan: "4" }, text: "暂无分桶数据" }),
        ])
      );
      return;
    }

    for (const b of series) {
      if (!b || typeof b !== "object") continue;
      const total = toFiniteNum(b.request_count) || 0;
      const fail = toFiniteNum(b.failure_count) || 0;
      const rateText = total > 0 ? formatPercent(Math.max(0, total - fail) / total) : "暂无";

      tbody.appendChild(
        createEl("tr", null, [
          createEl("th", {
            attrs: { scope: "row" },
            className: "col-sticky measured",
            text: formatBucketSpan(b.bucket_start_ms, b.bucket_end_ms, range, granularity),
          }),
          createEl("td", { className: "num", text: formatInt(total) }),
          createEl("td", { className: "num" + (fail > 0 ? " text-bad" : ""), text: formatInt(fail) }),
          createEl("td", { className: "num", text: rateText }),
        ])
      );
    }
  }

  function renderRankings(rankings, hasUsageData, hasError) {
    const configs = [
      {
        key: "models",
        list: rankings && Array.isArray(rankings.models) ? rankings.models : [],
        countId: "count-models",
        emptyId: "rank-models-empty",
        scrollId: "rank-models-scroll",
        bodyId: "rank-models-body",
        footerId: "rank-models-footer",
        emptyText: "该时间范围内暂无模型用量记录",
        getEntity: (row) => ({
          name: row && row.model ? String(row.model) : "未知模型",
          deleted: false,
        }),
      },
      {
        key: "groups",
        list: rankings && Array.isArray(rankings.groups) ? rankings.groups : [],
        countId: "count-groups",
        emptyId: "rank-groups-empty",
        scrollId: "rank-groups-scroll",
        bodyId: "rank-groups-body",
        footerId: "rank-groups-footer",
        emptyText: "该时间范围内暂无分组用量记录",
        getEntity: (row) => {
          const g = row && row.group && typeof row.group === "object" ? row.group : {};
          return {
            name: g.name ? String(g.name) : "未命名分组",
            deleted: Boolean(g.deleted),
          };
        },
      },
    ];

    for (const cfg of configs) {
      const countEl = byId(cfg.countId);
      const emptyEl = byId(cfg.emptyId);
      const scrollEl = byId(cfg.scrollId);
      const bodyEl = byId(cfg.bodyId);
      const footerEl = byId(cfg.footerId);

      if (countEl) countEl.textContent = String(cfg.list.length);
      clearChildren(bodyEl);

      if (!hasUsageData) {
        if (emptyEl) {
          emptyEl.textContent = hasError ? "暂无排行数据" : "正在加载排行数据…";
          emptyEl.hidden = false;
        }
        if (scrollEl) scrollEl.hidden = true;
        if (footerEl) footerEl.hidden = true;
        continue;
      }

      if (cfg.list.length === 0) {
        if (emptyEl) {
          emptyEl.textContent = cfg.emptyText;
          emptyEl.hidden = false;
        }
        if (scrollEl) scrollEl.hidden = true;
        if (footerEl) footerEl.hidden = true;
        continue;
      }

      if (emptyEl) emptyEl.hidden = true;
      if (scrollEl) scrollEl.hidden = false;

      const expanded = Boolean(state.expandedRankTabs[cfg.key]);
      const visibleRows = expanded ? cfg.list : cfg.list.slice(0, RANKING_DEFAULT_LIMIT);

      for (const row of visibleRows) {
        if (!row || typeof row !== "object") continue;
        const entity = cfg.getEntity(row);
        const entityChildren = [
          createEl("span", {
            className: "cell-entity-name",
            text: entity.name,
            attrs: { title: entity.name },
          }),
        ];
        if (entity.deleted) {
          entityChildren.push(createEl("span", { className: "badge badge--neutral", text: "已删除" }));
        }

        bodyEl.appendChild(
          createEl("tr", null, [
            createEl("th", { attrs: { scope: "row" }, className: "col-sticky" }, [
              createEl("span", { className: "cell-entity" }, entityChildren),
            ]),
            createEl("td", { className: "num", text: formatInt(row.request_count) }),
            createEl("td", { className: "num", text: formatCompactInt(row.total_tokens) }),
            createEl("td", { className: "num", text: formatUsdFromNano(row.estimated_cost_nano_usd) }),
          ])
        );
      }

      if (footerEl) {
        if (cfg.list.length > RANKING_DEFAULT_LIMIT) {
          const btn = footerEl.querySelector("button");
          if (btn) {
            btn.textContent = expanded
              ? `收起至前 ${RANKING_DEFAULT_LIMIT} 项`
              : `展开全部（共 ${cfg.list.length} 项）`;
          }
          footerEl.hidden = false;
        } else {
          footerEl.hidden = true;
        }
      }
    }
  }

  function renderQuotaSection() {
    const quotaState = state.sections.quota;
    const sectionEl = byId("section-quota");
    const countBadgeEl = byId("quota-count-badge");
    const metaEl = byId("quota-meta");
    const emptyEl = byId("quota-empty");
    const listEl = byId("quota-list");

    if (sectionEl) {
      sectionEl.setAttribute("aria-busy", String(state.isLoading && !quotaState.data));
    }

    setAlert(
      byId("quota-error"),
      quotaState.error
        ? quotaState.data
          ? `订阅额度获取失败：${quotaState.error}（当前保留上次成功数据）`
          : `订阅额度获取失败：${quotaState.error}`
        : null
    );

    const data = quotaState.data;
    const items = data && Array.isArray(data.items) ? data.items : [];

    if (countBadgeEl) countBadgeEl.textContent = `${items.length} 项`;
    if (metaEl) {
      const parts = [];
      if (quotaState.updatedAtMs) {
        parts.push(`更新 ${formatLocalDateTime(quotaState.updatedAtMs)}`);
      }
      if (data && toFiniteNum(data.observed_at_ms)) {
        parts.push(`上游观测 ${formatLocalDateTime(data.observed_at_ms)}`);
      }
      if (items.length > 0) parts.push("金额按 GPT-Load 已计价用量估算");
      metaEl.textContent = parts.length > 0 ? parts.join("  |  ") : (state.isLoading ? "加载中…" : "暂无更新时间");
    }

    clearChildren(listEl);

    if (!data) {
      if (emptyEl) {
        emptyEl.textContent = quotaState.error ? "暂无可用订阅额度数据" : "正在加载订阅额度…";
        emptyEl.hidden = false;
      }
      if (listEl) listEl.hidden = true;
      return;
    }

    if (items.length === 0) {
      if (emptyEl) {
        emptyEl.textContent = "暂无订阅额度记录";
        emptyEl.hidden = false;
      }
      if (listEl) listEl.hidden = true;
      return;
    }

    if (emptyEl) emptyEl.hidden = true;
    if (listEl) listEl.hidden = false;

    const nowMs = Date.now();

    for (const item of items) {
      if (!item || typeof item !== "object") continue;

      const cred = item.credential && typeof item.credential === "object" ? item.credential : {};
      const obs = cred.observation && typeof cred.observation === "object" ? cred.observation : {};
      const snap = obs.snapshot && typeof obs.snapshot === "object" ? obs.snapshot : null;
      const planSummary = snap && snap.plan_summary && typeof snap.plan_summary === "object" ? snap.plan_summary : null;
      const windows = snap && Array.isArray(snap.quota_windows) ? snap.quota_windows : [];

      const credId = cred.credential_id != null && cred.credential_id !== "" ? String(cred.credential_id) : "—";
      const channelName = item.channel_name ? String(item.channel_name) : "未命名渠道";
      const planName = planSummary && planSummary.name ? String(planSummary.name) : "未识别套餐";
      const statusBadge = mapStatusBadge(cred.effective_status);

      const groupCount = toFiniteNum(item.group_count);
      const availGroupCount = toFiniteNum(item.available_group_count);
      const groupRatioText = groupCount !== null
        ? `${formatInt(availGroupCount)} / ${formatInt(groupCount)}`
        : "—";

      const obsTimeMs = toFiniteNum(obs.observed_at_ms) || toFiniteNum(data.observed_at_ms);

      const badges = [
        createEl("span", { className: `badge badge--${statusBadge.tone}`, text: statusBadge.text }),
      ];
      if (obs.state && String(obs.state).toLowerCase() !== "ok") {
        badges.push(createEl("span", { className: "badge badge--neutral", text: `观测：${String(obs.state)}` }));
      }

      const metaList = createEl("dl", { className: "quota-meta-list" }, [
        createEl("div", { className: "quota-meta-pair" }, [
          createEl("dt", { text: "套餐" }),
          createEl("dd", { text: planName }),
        ]),
        createEl("div", { className: "quota-meta-pair" }, [
          createEl("dt", { text: "可用分组" }),
          createEl("dd", { className: "measured", text: groupRatioText }),
        ]),
        createEl("div", { className: "quota-meta-pair" }, [
          createEl("dt", { text: "上游观测" }),
          createEl("dd", { className: "measured", text: obsTimeMs ? formatLocalDateTime(obsTimeMs) : "—" }),
        ]),
      ]);

      const windowsContainer = createEl("div", { className: "quota-windows" });
      if (windows.length === 0) {
        const emptyWinText = obs.state && String(obs.state).toLowerCase() !== "ok"
          ? `暂无额度窗口快照（观测状态：${String(obs.state)}）`
          : "暂无额度窗口快照";
        windowsContainer.appendChild(createEl("p", { className: "quota-empty-windows", text: emptyWinText }));
      } else {
        for (const win of windows) {
          if (!win || typeof win !== "object") continue;
          windowsContainer.appendChild(buildQuotaWindowNode(win, nowMs));
        }
      }

      const card = createEl("article", { className: "quota-item" }, [
        createEl("div", { className: "quota-item-header" }, [
          createEl("div", { className: "quota-identity" }, [
            createEl("h3", { className: "quota-account measured", text: `订阅账号 #${credId}` }),
            createEl("p", { className: "quota-channel", text: `渠道：${channelName}` }),
          ]),
          createEl("div", { className: "quota-badges" }, badges),
        ]),
        metaList,
        windowsContainer,
      ]);

      listEl.appendChild(card);
    }
  }

  function isWeeklyQuotaWindow(win) {
    if (!win || typeof win !== "object") return false;
    const secs = toFiniteNum(win.window_seconds);
    if (secs === 604800) return true;
    const descriptor = `${win.id || ""} ${win.label || ""}`;
    return /(?:^|[^a-z0-9])7d(?:[^a-z0-9]|$)/i.test(descriptor) || /7\s*天/.test(descriptor);
  }

  function buildQuotaStatCell(label, valueText, extraClass) {
    const isUnavailable = valueText === "暂无";
    return createEl("div", { className: extraClass ? `quota-stat ${extraClass}` : "quota-stat" }, [
      createEl("span", { className: "quota-stat-label", text: label }),
      createEl("span", {
        className: isUnavailable ? "quota-stat-value measured quota-stat-value--unavailable" : "quota-stat-value measured",
        text: valueText,
        attrs: { title: valueText },
      }),
    ]);
  }

  function buildQuotaWindowNode(win, nowMs) {
    const unit = win.unit ? String(win.unit) : "";
    const durText = formatWindowDuration(win.window_seconds);
    const baseLabel = win.label
      ? String(win.label)
      : (durText || (win.scope ? String(win.scope) : (win.id ? String(win.id) : "额度窗口")));

    let titleText = baseLabel;
    if (durText && !baseLabel.includes(durText) && !(win.window_seconds === 18000 && /5h/i.test(baseLabel)) && !(win.window_seconds === 604800 && /7d/i.test(baseLabel))) {
      titleText = `${baseLabel}（${durText}）`;
    }
    if (win.scope && !titleText.toLowerCase().includes(String(win.scope).toLowerCase())) {
      titleText = `${titleText} [${String(win.scope)}]`;
    }

    // Utilization ratio 0..1 (fallback to used/limit only if both are finite upstream numbers)
    let ratio = toFiniteNum(win.utilization);
    const usedNum = toFiniteNum(win.used);
    const limitNum = toFiniteNum(win.limit);
    const remainingNum = toFiniteNum(win.remaining);

    if (ratio === null && usedNum !== null && limitNum !== null && limitNum > 0) {
      ratio = usedNum / limitNum;
    }

    let pctText = "—";
    let progressClass = "quota-progress quota-progress--unknown";
    let progressVal = 0;

    if (ratio !== null) {
      const clamped = Math.max(0, Math.min(1, ratio));
      progressVal = clamped;
      pctText = formatPercent(ratio);
      if (ratio >= 0.9) {
        progressClass = "quota-progress quota-progress--crit";
      } else if (ratio >= 0.7) {
        progressClass = "quota-progress quota-progress--warn";
      } else {
        progressClass = "quota-progress";
      }
    }

    const usedAmountUsd = toFiniteNum(win.used_amount_usd);
    const estimatedLimitUsd = toFiniteNum(win.estimated_limit_usd);
    const safeLevelPercent = toFiniteNum(win.safe_level_percent);
    const safeAmountUsd = toFiniteNum(win.safe_amount_usd);
    const aheadAmountUsd = toFiniteNum(win.ahead_amount_usd);
    const pricingIncomplete = Boolean(win.pricing_incomplete);

    const limitLabel = isWeeklyQuotaWindow(win) ? "预估周额度" : "预估额度";
    const isOverSafeLine = Boolean(
      (aheadAmountUsd !== null && aheadAmountUsd > 0) ||
      (ratio !== null && safeLevelPercent !== null && (ratio * 100) > safeLevelPercent) ||
      (safeAmountUsd !== null && safeAmountUsd < 0)
    );

    const safeLabel = isOverSafeLine ? "超出安全额度" : "安全额度";
    const safeValueText = isOverSafeLine
      ? formatUsdFixed2(
          aheadAmountUsd !== null
            ? aheadAmountUsd
            : (safeAmountUsd !== null && safeAmountUsd < 0 ? Math.abs(safeAmountUsd) : null)
        )
      : formatUsdFixed2(safeAmountUsd);
    const safeCellClass = isOverSafeLine
      ? "quota-stat--over"
      : (safeAmountUsd !== null ? "quota-stat--safe" : "");

    const detailParts = [];
    const usedFmt = formatRawUnitValue(usedNum, unit);
    const limitFmt = formatRawUnitValue(limitNum, unit);
    const remFmt = formatRawUnitValue(remainingNum, unit);

    if (usedFmt !== null && limitFmt !== null) {
      detailParts.push(`已用 ${usedFmt} / ${limitFmt}`);
    } else if (usedFmt !== null) {
      detailParts.push(`已用 ${usedFmt}`);
    }
    if (remFmt !== null) {
      detailParts.push(`剩余 ${remFmt}`);
    }
    if (win.state && String(win.state).toLowerCase() !== "ok") {
      detailParts.push(`状态 ${String(win.state)}`);
    }

    const resetMs = toFiniteNum(win.reset_at_ms);
    const countdownEl = createEl("span", {
      className: "quota-countdown measured",
      text: formatResetCountdown(resetMs, nowMs),
      dataset: resetMs && resetMs > 0 ? { resetAtMs: String(Math.round(resetMs)) } : null,
    });

    const progressAttrs = {
      max: "1",
      value: String(progressVal),
      "aria-label": `${titleText} 已使用 ${pctText}`,
    };
    const progress = createEl("div", { className: "quota-progress-track" }, [
      createEl("progress", { className: progressClass, attrs: progressAttrs }),
    ]);
    if (safeLevelPercent !== null) {
      const marker = createEl("span", {
        className: "quota-safety-marker",
        attrs: { "aria-hidden": "true", title: `安全线 ${formatSafeLevelPercent(safeLevelPercent)}` },
      });
      marker.style.setProperty("--safety-position", `${Math.max(0, Math.min(100, safeLevelPercent))}%`);
      progress.appendChild(marker);
    }

    const titleGroupChildren = [
      createEl("span", { className: "quota-window-label", text: titleText }),
    ];
    if (pricingIncomplete) {
      titleGroupChildren.push(
        createEl("span", {
          className: "badge badge--warn",
          text: "计价不全",
          attrs: { title: "部分请求缺少定价，估算金额可能不完整" },
        })
      );
    }

    const metricsGrid = createEl("div", { className: "quota-window-metrics" }, [
      buildQuotaStatCell("已用金额", formatUsdFixed2(usedAmountUsd), "quota-stat--primary"),
      buildQuotaStatCell(limitLabel, formatUsdFixed2(estimatedLimitUsd), "quota-stat--primary"),
      buildQuotaStatCell("安全线", formatSafeLevelPercent(safeLevelPercent), ""),
      buildQuotaStatCell(safeLabel, safeValueText, safeCellClass),
    ]);

    return createEl("div", { className: "quota-window" }, [
      createEl("div", { className: "quota-window-top" }, [
        createEl("span", { className: "quota-window-title-group" }, titleGroupChildren),
        createEl("span", { className: "quota-window-pct measured", text: `已用 ${pctText}` }),
      ]),
      progress,
      metricsGrid,
      createEl("div", { className: "quota-window-bottom" }, [
        createEl("span", { className: "measured", text: detailParts.join("，") }),
        countdownEl,
      ]),
    ]);
  }

  function tickCountdowns() {
    const nodes = document.querySelectorAll("[data-reset-at-ms]");
    if (nodes.length === 0) return;
    const nowMs = Date.now();
    for (const node of nodes) {
      const target = toFiniteNum(node.dataset.resetAtMs);
      if (target !== null) {
        node.textContent = formatResetCountdown(target, nowMs);
      }
    }
  }

  /* ------------------------------------------------------------------------
     Canvas Request Trend Chart (Zero Baseline, DPR-aware, Keyboard & Pointer)
     ------------------------------------------------------------------------ */

  function getChartColors() {
    const styles = getComputedStyle(document.documentElement);
    return {
      grid: styles.getPropertyValue("--chart-grid").trim() || "#3a3e47",
      axisText: styles.getPropertyValue("--chart-axis-text").trim() || "#9ea4b0",
      total: styles.getPropertyValue("--chart-series-total").trim() || "#7fb2f0",
      totalHover: styles.getPropertyValue("--chart-series-total-hover").trim() || "#9cc6f8",
      failure: styles.getPropertyValue("--chart-series-failure").trim() || "#e06c64",
      highlight: styles.getPropertyValue("--chart-highlight-band").trim() || "#2d323b",
      monoFont: styles.getPropertyValue("--font-outlier").trim() || "monospace",
    };
  }

  function computeNiceMax(maxVal) {
    if (!Number.isFinite(maxVal) || maxVal <= 0) return 4;
    const exp = Math.floor(Math.log10(maxVal));
    const base = Math.pow(10, exp);
    const frac = maxVal / base;
    let niceFrac;
    if (frac <= 1) niceFrac = 1;
    else if (frac <= 2) niceFrac = 2;
    else if (frac <= 4) niceFrac = 4;
    else if (frac <= 5) niceFrac = 5;
    else niceFrac = 10;
    const nice = niceFrac * base;
    return nice < 4 ? 4 : nice;
  }

  function formatAxisTick(val) {
    if (val >= 1e6) return (val / 1e6).toFixed(val % 1e6 === 0 ? 0 : 1) + "M";
    if (val >= 1e3) return (val / 1e3).toFixed(val % 1e3 === 0 ? 0 : 1) + "K";
    return String(Math.round(val));
  }

  function updateChartInspectText() {
    const inspectEl = byId("usage-chart-inspect");
    if (!inspectEl) return;
    const series = state.chart.series;
    if (!series || series.length === 0) {
      inspectEl.textContent = "该时间范围内暂无请求趋势分桶数据。";
      return;
    }

    const activeIdx = state.chart.hoveredIndex >= 0 ? state.chart.hoveredIndex : state.chart.focusedIndex;
    if (activeIdx >= 0 && activeIdx < series.length) {
      const b = series[activeIdx];
      const total = toFiniteNum(b.request_count) || 0;
      const fail = toFiniteNum(b.failure_count) || 0;
      const ok = Math.max(0, total - fail);
      const rate = total > 0 ? formatPercent(ok / total) : "暂无";
      const span = formatBucketSpan(b.bucket_start_ms, b.bucket_end_ms, state.chart.range, state.chart.granularity);
      inspectEl.textContent = `${span}  |  总请求 ${formatInt(total)}  |  成功 ${formatInt(ok)}  |  失败 ${formatInt(fail)}  |  成功率 ${rate}`;
      return;
    }

    let maxBucketReq = 0;
    let totalFail = 0;
    for (const b of series) {
      const r = toFiniteNum(b && b.request_count) || 0;
      const f = toFiniteNum(b && b.failure_count) || 0;
      if (r > maxBucketReq) maxBucketReq = r;
      totalFail += f;
    }
    if (maxBucketReq === 0) {
      inspectEl.textContent = `共 ${series.length} 个时间分桶，当前范围内请求数均为 0。`;
    } else {
      inspectEl.textContent = `共 ${series.length} 个时间分桶，峰值单桶 ${formatInt(maxBucketReq)} 次请求，累计失败 ${formatInt(totalFail)} 次（移动指针或按左右键查看明细）。`;
    }
  }

  function renderTrendChart() {
    const canvas = byId("usage-trend-canvas");
    const emptyEl = byId("usage-chart-empty");
    if (!canvas) return;

    const series = state.chart.series;
    updateChartInspectText();

    if (!series || series.length === 0) {
      canvas.hidden = true;
      if (emptyEl) {
        const usageState = state.usageByRange[state.range];
        const hasErr = Boolean(usageState.error || state.globalError);
        emptyEl.textContent = state.isLoading && !usageState.data
          ? `正在加载${rangeLabel(state.range)}请求趋势…`
          : (hasErr && !usageState.data ? "请求趋势加载失败" : "该时间范围内暂无请求趋势数据");
        emptyEl.hidden = false;
      }
      state.chart.bars = [];
      return;
    }

    canvas.hidden = false;
    if (emptyEl) emptyEl.hidden = true;

    const rect = canvas.getBoundingClientRect();
    const cssWidth = Math.max(240, Math.floor(rect.width || 600));
    const cssHeight = Math.max(180, Math.floor(rect.height || 220));
    const dpr = Math.min(window.devicePixelRatio || 1, 2);

    if (canvas.width !== cssWidth * dpr || canvas.height !== cssHeight * dpr) {
      canvas.width = cssWidth * dpr;
      canvas.height = cssHeight * dpr;
    }

    const ctx = canvas.getContext("2d");
    if (!ctx) return;

    ctx.save();
    ctx.scale(dpr, dpr);
    ctx.clearRect(0, 0, cssWidth, cssHeight);

    const colors = getChartColors();
    const padLeft = 46;
    const padRight = 14;
    const padTop = 12;
    const padBottom = 28;
    const plotW = Math.max(10, cssWidth - padLeft - padRight);
    const plotH = Math.max(10, cssHeight - padTop - padBottom);

    let maxReq = 0;
    for (const b of series) {
      const r = toFiniteNum(b && b.request_count) || 0;
      const f = toFiniteNum(b && b.failure_count) || 0;
      if (r > maxReq) maxReq = r;
      if (f > maxReq) maxReq = f;
    }

    const yMax = computeNiceMax(maxReq);
    const tickCount = 4;

    // Horizontal hairline gridlines + Y-axis labels (zero baseline always)
    ctx.font = `11px ${colors.monoFont}`;
    ctx.textAlign = "right";
    ctx.textBaseline = "middle";
    ctx.lineWidth = 1;

    for (let i = 0; i <= tickCount; i++) {
      const ratio = i / tickCount;
      const val = yMax * (1 - ratio);
      const y = Math.round(padTop + plotH * ratio) + 0.5;

      ctx.strokeStyle = colors.grid;
      ctx.beginPath();
      ctx.moveTo(padLeft, y);
      ctx.lineTo(cssWidth - padRight, y);
      ctx.stroke();

      ctx.fillStyle = colors.axisText;
      ctx.fillText(formatAxisTick(val), padLeft - 8, y);
    }

    // Bars geometry
    const count = series.length;
    const slotW = plotW / count;
    const gap = count > 40 ? 1 : (slotW > 14 ? 4 : 2);
    const barW = Math.max(2, Math.min(24, slotW - gap));
    const activeIdx = state.chart.hoveredIndex >= 0 ? state.chart.hoveredIndex : state.chart.focusedIndex;

    const barRects = [];
    const baseY = padTop + plotH;

    for (let i = 0; i < count; i++) {
      const b = series[i] || {};
      const total = Math.max(0, toFiniteNum(b.request_count) || 0);
      const fail = Math.max(0, toFiniteNum(b.failure_count) || 0);

      const slotX = padLeft + i * slotW;
      const x = slotX + (slotW - barW) / 2;

      barRects.push({ index: i, slotX, slotW });

      // Subtle vertical highlight band for hovered/focused bucket
      if (i === activeIdx) {
        ctx.fillStyle = colors.highlight;
        ctx.fillRect(slotX, padTop, slotW, plotH);
      }

      if (total > 0) {
        const h = Math.max(2, (total / yMax) * plotH);
        const y = baseY - h;
        ctx.fillStyle = i === activeIdx ? colors.totalHover : colors.total;
        drawTopRoundedRect(ctx, x, y, barW, h, Math.min(2, barW / 2));
      }

      if (fail > 0) {
        const fh = Math.max(2, (fail / yMax) * plotH);
        const fy = baseY - fh;
        ctx.fillStyle = colors.failure;
        ctx.fillRect(x, fy, barW, fh);
      }
    }

    state.chart.bars = barRects;

    // Space labels evenly between both ends so the last two stay readable on mobile.
    const wideAxisLabel = !isDayGranularity(state.chart.range, state.chart.granularity) &&
      (state.chart.range === "7d" || state.chart.range === "30d");
    const minLabelSpacing = wideAxisLabel ? 88 : 64;
    const edgeInset = wideAxisLabel ? 36 : 18;
    const labelCount = Math.min(count, Math.max(2, 1 + Math.floor((plotW - edgeInset * 2) / minLabelSpacing)));
    ctx.fillStyle = colors.axisText;
    ctx.textAlign = "center";
    ctx.textBaseline = "top";

    for (let labelIndex = 0; labelIndex < labelCount; labelIndex++) {
      const i = labelCount === 1 ? 0 : Math.round(labelIndex * (count - 1) / (labelCount - 1));
      const b = series[i];
      const label = formatBucketAxisLabel(b && b.bucket_start_ms, state.chart.range, state.chart.granularity);
      if (!label) continue;
      const cx = padLeft + i * slotW + slotW / 2;
      const clampedX = Math.max(padLeft + edgeInset, Math.min(cssWidth - padRight - edgeInset, cx));
      ctx.fillText(label, clampedX, baseY + 8);
    }

    ctx.restore();
  }

  function drawTopRoundedRect(ctx, x, y, w, h, r) {
    const radius = Math.min(r, h, w / 2);
    if (radius <= 0.5) {
      ctx.fillRect(x, y, w, h);
      return;
    }
    ctx.beginPath();
    ctx.moveTo(x, y + h);
    ctx.lineTo(x, y + radius);
    ctx.quadraticCurveTo(x, y, x + radius, y);
    ctx.lineTo(x + w - radius, y);
    ctx.quadraticCurveTo(x + w, y, x + w, y + radius);
    ctx.lineTo(x + w, y + h);
    ctx.closePath();
    ctx.fill();
  }

  function findChartIndexAtClientX(canvas, clientX) {
    const rect = canvas.getBoundingClientRect();
    const x = clientX - rect.left;
    for (const bar of state.chart.bars) {
      if (x >= bar.slotX && x <= bar.slotX + bar.slotW) {
        return bar.index;
      }
    }
    return -1;
  }

  /* ------------------------------------------------------------------------
     AccessKey Cache-Hit Trend Chart (Multi-Series Line, 0-100%, Gaps, Toggle)
     ------------------------------------------------------------------------ */

  const ACCESS_KEY_FALLBACK_COLORS = [
    "#7ab3f0",
    "#5ec2ba",
    "#e5b95c",
    "#e47c67",
    "#b78ef0",
    "#6cc98a",
    "#e67ba8",
    "#64c9e0",
    "#b9cc5e",
    "#8e9df0",
    "#e89e64",
    "#d68de0",
  ];

  function getAccessKeyDisplayName(item) {
    const rawName = item && item.name != null ? String(item.name).trim() : "";
    if (rawName) return rawName;
    if (item && item.id != null && item.id !== "") return `AccessKey #${item.id}`;
    return "未命名 AccessKey";
  }

  function getAccessKeyColorIndex(id) {
    const num = toFiniteNum(id);
    if (num !== null) {
      return Math.abs(Math.trunc(num)) % ACCESS_KEY_PALETTE_SIZE;
    }
    const str = String(id == null ? "" : id);
    let hash = 0;
    for (let i = 0; i < str.length; i++) {
      hash = ((hash * 31) + str.charCodeAt(i)) >>> 0;
    }
    return hash % ACCESS_KEY_PALETTE_SIZE;
  }

  function assignAccessKeyColors(rows) {
    const assigned = new Set();
    const previous = state.accessKeyCacheChart.colorByKeyId;
    const pending = [];
    for (const row of rows) {
      const color = previous.get(row.keyId);
      if (color === undefined || assigned.has(color)) {
        pending.push(row);
      } else {
        row.colorIndex = color;
        assigned.add(color);
      }
    }
    for (const row of pending) {
      const preferred = getAccessKeyColorIndex(row.keyId);
      let color = preferred;
      while (assigned.size < ACCESS_KEY_PALETTE_SIZE && assigned.has(color)) {
        color = (color + 1) % ACCESS_KEY_PALETTE_SIZE;
      }
      row.colorIndex = color;
      previous.set(row.keyId, color);
      assigned.add(color);
    }
  }

  function getAccessKeyChartColors() {
    const styles = getComputedStyle(document.documentElement);
    const palette = [];
    for (let i = 0; i < ACCESS_KEY_PALETTE_SIZE; i++) {
      palette.push(
        styles.getPropertyValue(`--ak-series-${i}`).trim() ||
        ACCESS_KEY_FALLBACK_COLORS[i]
      );
    }
    return {
      grid: styles.getPropertyValue("--chart-grid").trim() || "#3a3e47",
      axisText: styles.getPropertyValue("--chart-axis-text").trim() || "#9ea4b0",
      crosshair: styles.getPropertyValue("--chart-crosshair").trim() || "#6f7785",
      pointRing: styles.getPropertyValue("--chart-point-ring").trim() || "#1b1d22",
      monoFont: styles.getPropertyValue("--font-outlier").trim() || "monospace",
      palette,
    };
  }

  function formatBucketWidthLabel(widthMs) {
    const w = toFiniteNum(widthMs);
    if (w === null || w <= 0) return "";
    const rounded = Math.round(w);
    const dayMs = 86400000;
    const hourMs = 3600000;
    const minMs = 60000;
    if (rounded >= dayMs && rounded % dayMs === 0) {
      const days = rounded / dayMs;
      return days === 1 ? "按天" : `每 ${days} 天`;
    }
    if (rounded >= hourMs && rounded % hourMs === 0) {
      const hours = rounded / hourMs;
      return hours === 1 ? "按小时" : `每 ${hours} 小时`;
    }
    if (rounded >= minMs && rounded % minMs === 0) {
      const mins = rounded / minMs;
      return mins === 1 ? "按分钟" : `每 ${mins} 分钟`;
    }
    return "";
  }

  function buildAccessKeyCacheChartModel(data, range) {
    const items = data && Array.isArray(data.items) ? data.items : [];
    const eligibleRows = [];
    const bucketMap = new Map();

    for (const item of items) {
      if (!item || typeof item !== "object" || item.stale) continue;
      if (!Array.isArray(item.cache_hit_series) || item.cache_hit_series.length === 0) continue;

      const pointsByStart = new Map();
      const rowBuckets = [];
      let hasValidPoint = false;

      for (const pt of item.cache_hit_series) {
        if (!pt || typeof pt !== "object") continue;
        const startMs = toFiniteNum(pt.bucket_start_ms);
        if (startMs === null) continue;
        const endMs = toFiniteNum(pt.bucket_end_ms);
        const rate = toFiniteNum(pt.cache_hit_rate);
        if (rate !== null) hasValidPoint = true;
        pointsByStart.set(startMs, rate);
        rowBuckets.push({ startMs, endMs });
      }

      if (!hasValidPoint) continue;

      for (const b of rowBuckets) {
        const existing = bucketMap.get(b.startMs);
        if (!existing || (existing.endMs === null && b.endMs !== null)) {
          bucketMap.set(b.startMs, b);
        }
      }

      const name = getAccessKeyDisplayName(item);
      const hasId = item.id != null && item.id !== "";
      const keyId = hasId ? String(item.id) : name;
      eligibleRows.push({
        keyId,
        id: item.id,
        name,
        deleted: Boolean(item.deleted),
        pointsByStart,
      });
    }

    const buckets = Array.from(bucketMap.values()).sort((a, b) => a.startMs - b.startMs);
    assignAccessKeyColors(eligibleRows);
    const series = eligibleRows.map((row) => ({
      keyId: row.keyId,
      id: row.id,
      name: row.name,
      deleted: row.deleted,
      colorIndex: row.colorIndex,
      values: buckets.map((b) => (row.pointsByStart.has(b.startMs) ? row.pointsByStart.get(b.startMs) : null)),
    }));

    let bucketWidthMs = null;
    for (const b of buckets) {
      if (b.endMs !== null && b.endMs > b.startMs) {
        const span = b.endMs - b.startMs;
        if (bucketWidthMs === null || span > bucketWidthMs) {
          bucketWidthMs = span;
        }
      }
    }
    if (bucketWidthMs === null && buckets.length > 1) {
      const diffs = [];
      for (let i = 1; i < buckets.length; i++) {
        const d = buckets[i].startMs - buckets[i - 1].startMs;
        if (d > 0) diffs.push(d);
      }
      if (diffs.length > 0) {
        diffs.sort((a, b) => a - b);
        bucketWidthMs = diffs[Math.floor(diffs.length / 2)];
      }
    }

    const isDay = bucketWidthMs !== null
      ? bucketWidthMs >= 20 * 3600 * 1000
      : range === "30d";

    return { buckets, series, bucketWidthMs, isDay };
  }

  function getVisibleAccessKeyCacheSeries() {
    const chart = state.accessKeyCacheChart;
    return chart.series.filter((s) => !chart.hiddenKeyIds.has(s.keyId));
  }

  function formatAccessKeyBucketSpan(bucket, range) {
    if (!bucket) return "—";
    return formatBucketSpan(bucket.startMs, bucket.endMs, range, "hour");
  }

  function formatAccessKeyAxisLabel(startMs, range, isDay, spanMs) {
    const p = getBeijingParts(startMs);
    if (!p) return "";
    if (isDay) return `${p.month}-${p.day}`;
    if (range === "7d" || range === "30d" || spanMs > 36 * 3600 * 1000) {
      return `${p.month}-${p.day} ${p.hour}:${p.minute}`;
    }
    return `${p.hour}:${p.minute}`;
  }

  function setAccessKeyCacheLiveText(text) {
    const liveEl = byId("access-keys-cache-live");
    if (!liveEl) return;
    liveEl.textContent = text || "";
  }

  function renderAccessKeyCacheSection(activeRange, akState) {
    const chart = state.accessKeyCacheChart;
    const data = akState && akState.data ? akState.data : null;
    const model = data
      ? buildAccessKeyCacheChartModel(data, activeRange)
      : { buckets: [], series: [], bucketWidthMs: null, isDay: activeRange === "30d" };

    const rangeChanged = chart.range !== activeRange;
    const focusedBucket = rangeChanged ? null : chart.buckets[chart.focusedIndex];
    const first = model.buckets[0];
    const last = model.buckets[model.buckets.length - 1];
    chart.range = activeRange;
    chart.fromMs = toFiniteNum(data?.from_ms) ?? first?.startMs ?? null;
    chart.toMs = toFiniteNum(data?.to_ms) ?? last?.endMs ?? last?.startMs ?? null;
    chart.buckets = model.buckets;
    chart.series = model.series;
    chart.bucketWidthMs = model.bucketWidthMs;
    chart.isDay = model.isDay;
    chart.hoveredIndex = -1;

    chart.focusedIndex = focusedBucket ? chart.buckets.findIndex((bucket) =>
      bucket.startMs === focusedBucket.startMs ||
      (focusedBucket.endMs !== null && bucket.endMs === focusedBucket.endMs)
    ) : -1;
    if (chart.focusedIndex < 0) {
      chart.interactionSource = null;
      setAccessKeyCacheLiveText("");
    }

    const winLabelEl = byId("access-keys-cache-window-label");
    if (winLabelEl) {
      const { fromMs, toMs } = chart;
      if (fromMs !== null && toMs !== null && toMs >= fromMs) {
        const granText = formatBucketWidthLabel(chart.bucketWidthMs);
        const gran = granText ? `（${granText}）` : "";
        winLabelEl.textContent = `${formatLocalShortTime(fromMs)} 至 ${formatLocalShortTime(toMs)}${gran}`;
      } else {
        winLabelEl.textContent = rangeLabel(activeRange);
      }
    }

    renderAccessKeyCacheLegend();
    renderAccessKeyCacheChart(false);
    renderAccessKeyCacheTable();
  }

  function renderAccessKeyCacheLegend() {
    const legendEl = byId("access-keys-cache-legend");
    if (!legendEl) return;
    const chart = state.accessKeyCacheChart;

    if (chart.series.length === 0) {
      clearChildren(legendEl);
      legendEl.hidden = true;
      chart.legendSignature = "";
      return;
    }

    legendEl.hidden = false;
    const nextSignature = chart.series
      .map((s) => `${s.keyId}|${s.name}|${s.colorIndex}|${s.deleted ? 1 : 0}`)
      .join("\n");

    if (nextSignature === chart.legendSignature) {
      const buttons = legendEl.querySelectorAll("button[data-cache-key-id]");
      for (const btn of buttons) {
        const isVisible = !chart.hiddenKeyIds.has(btn.dataset.cacheKeyId);
        btn.setAttribute("aria-pressed", String(isVisible));
      }
      return;
    }

    const activeEl = document.activeElement;
    const focusedKeyId = activeEl && legendEl.contains(activeEl) && activeEl.dataset
      ? activeEl.dataset.cacheKeyId
      : null;

    clearChildren(legendEl);
    let buttonToRefocus = null;

    for (const s of chart.series) {
      const isVisible = !chart.hiddenKeyIds.has(s.keyId);
      const children = [
        createEl("span", {
          className: `ak-legend-swatch ak-series-color--${s.colorIndex}`,
          attrs: { "aria-hidden": "true" },
        }),
        createEl("span", {
          className: "ak-legend-name",
          text: s.name,
          attrs: { title: s.name },
        }),
      ];
      if (s.deleted) {
        children.push(createEl("span", { className: "ak-legend-tag", text: "已删除" }));
      }
      const btn = createEl("button", {
        className: "ak-legend-btn",
        attrs: {
          type: "button",
          "aria-pressed": String(isVisible),
        },
        dataset: { cacheKeyId: s.keyId },
      }, children);
      legendEl.appendChild(btn);
      if (focusedKeyId && s.keyId === focusedKeyId) {
        buttonToRefocus = btn;
      }
    }

    chart.legendSignature = nextSignature;
    if (buttonToRefocus) {
      buttonToRefocus.focus();
    }
  }

  function updateAccessKeyCacheInspect(visibleSeries, announceLive) {
    const inspectEl = byId("access-keys-cache-inspect");
    if (!inspectEl) return;
    const chart = state.accessKeyCacheChart;
    clearChildren(inspectEl);

    if (chart.series.length === 0 || chart.buckets.length === 0) {
      const akState = state.accessKeysByRange[state.range];
      const msg = state.isLoading && !(akState && akState.data)
        ? `正在加载${rangeLabel(state.range)}缓存命中率分桶数据…`
        : "该时间范围内暂无缓存命中率分桶数据。";
      inspectEl.textContent = msg;
      return;
    }

    if (visibleSeries.length === 0) {
      const msg = "已隐藏全部 AccessKey 曲线，点击上方图例可重新显示。";
      inspectEl.textContent = msg;
      if (announceLive) setAccessKeyCacheLiveText(msg);
      return;
    }

    const activeIdx = chart.hoveredIndex >= 0 ? chart.hoveredIndex : chart.focusedIndex;
    if (activeIdx >= 0 && activeIdx < chart.buckets.length) {
      const span = formatAccessKeyBucketSpan(chart.buckets[activeIdx], chart.range);
      const itemNodes = [];
      const liveParts = [];

      for (const s of visibleSeries) {
        const val = s.values[activeIdx];
        const hasVal = val !== null;
        const valText = hasVal ? formatPercent(val) : "无数据";
        liveParts.push(`${s.name} ${valText}`);
        itemNodes.push(
          createEl("span", { className: "ak-inspect-item" }, [
            createEl("span", {
              className: `ak-legend-swatch ak-series-color--${s.colorIndex}`,
              attrs: { "aria-hidden": "true" },
            }),
            createEl("span", {
              className: "ak-inspect-name",
              text: s.name,
              attrs: { title: s.name },
            }),
            createEl("span", {
              className: hasVal ? "ak-inspect-val measured" : "ak-inspect-val ak-inspect-val--empty",
              text: valText,
            }),
          ])
        );
      }

      inspectEl.appendChild(createEl("span", { className: "ak-inspect-time measured", text: span }));
      inspectEl.appendChild(createEl("span", { className: "ak-inspect-list" }, itemNodes));

      if (announceLive) {
        setAccessKeyCacheLiveText(`${span}：${liveParts.join("，")}`);
      }
      return;
    }

    inspectEl.textContent =
      `共 ${chart.buckets.length} 个时间分桶，显示 ${visibleSeries.length}/${chart.series.length} 条曲线（悬停、轻点或聚焦图表按左右方向键查看明细）。`;
  }

  function renderAccessKeyCacheChart(announceLive) {
    const canvas = byId("access-keys-cache-canvas");
    const emptyEl = byId("access-keys-cache-empty");
    if (!canvas) return;

    const chart = state.accessKeyCacheChart;
    const visibleSeries = getVisibleAccessKeyCacheSeries();
    updateAccessKeyCacheInspect(visibleSeries, Boolean(announceLive));

    if (chart.series.length === 0 || chart.buckets.length === 0) {
      canvas.hidden = true;
      if (emptyEl) {
        const akState = state.accessKeysByRange[state.range];
        const hasErr = Boolean((akState && akState.error) || state.globalError);
        emptyEl.textContent = state.isLoading && !(akState && akState.data)
          ? `正在加载${rangeLabel(state.range)}缓存命中率趋势…`
          : (hasErr && !(akState && akState.data) ? "缓存命中率趋势加载失败" : "该时间范围内暂无缓存命中率趋势数据");
        emptyEl.hidden = false;
      }
      chart.pointsX = [];
      return;
    }

    if (visibleSeries.length === 0) {
      canvas.hidden = true;
      if (emptyEl) {
        emptyEl.textContent = "已隐藏全部 AccessKey 曲线，点击上方图例可重新显示";
        emptyEl.hidden = false;
      }
      chart.pointsX = [];
      return;
    }

    canvas.hidden = false;
    if (emptyEl) emptyEl.hidden = true;

    const rect = canvas.getBoundingClientRect();
    const cssWidth = Math.max(200, Math.floor(rect.width || 600));
    const cssHeight = Math.max(180, Math.floor(rect.height || 220));
    const dpr = Math.min(window.devicePixelRatio || 1, 2);

    if (canvas.width !== cssWidth * dpr || canvas.height !== cssHeight * dpr) {
      canvas.width = cssWidth * dpr;
      canvas.height = cssHeight * dpr;
    }

    const ctx = canvas.getContext("2d");
    if (!ctx) return;

    ctx.save();
    ctx.scale(dpr, dpr);
    ctx.clearRect(0, 0, cssWidth, cssHeight);

    const colors = getAccessKeyChartColors();
    const padLeft = 42;
    const padRight = 12;
    const padTop = 12;
    const padBottom = 28;
    const plotW = Math.max(10, cssWidth - padLeft - padRight);
    const plotH = Math.max(10, cssHeight - padTop - padBottom);
    const baseY = padTop + plotH;

    const count = chart.buckets.length;
    const spanMs = chart.toMs - chart.fromMs;
    const pointsX = chart.buckets.map(({ startMs, endMs }) => {
      const midpoint = (startMs + (endMs ?? startMs)) / 2;
      const fraction = spanMs > 0 ? (midpoint - chart.fromMs) / spanMs : 0.5;
      return padLeft + Math.max(0, Math.min(1, fraction)) * plotW;
    });
    chart.pointsX = pointsX;
    chart.plotLeft = padLeft;
    chart.plotRight = cssWidth - padRight;

    // Fixed 0-100% horizontal dashed gridlines & Y-axis ticks (0/25/50/75/100)
    ctx.font = `11px ${colors.monoFont}`;
    ctx.textAlign = "right";
    ctx.textBaseline = "middle";
    ctx.lineWidth = 1;
    const yTicks = [100, 75, 50, 25, 0];

    for (let i = 0; i < yTicks.length; i++) {
      const ratio = i / (yTicks.length - 1);
      const y = Math.round(padTop + plotH * ratio) + 0.5;

      ctx.strokeStyle = colors.grid;
      ctx.setLineDash([2, 4]);
      ctx.beginPath();
      ctx.moveTo(padLeft, y);
      ctx.lineTo(cssWidth - padRight, y);
      ctx.stroke();

      ctx.fillStyle = colors.axisText;
      ctx.fillText(`${yTicks[i]}%`, padLeft - 8, y);
    }
    ctx.setLineDash([]);

    // Active bucket vertical guide hairline
    const activeIdx = chart.hoveredIndex >= 0 ? chart.hoveredIndex : chart.focusedIndex;
    if (activeIdx >= 0 && activeIdx < count) {
      const gx = Math.round(pointsX[activeIdx]) + 0.5;
      ctx.strokeStyle = colors.crosshair;
      ctx.lineWidth = 1;
      ctx.setLineDash([3, 3]);
      ctx.beginPath();
      ctx.moveTo(gx, padTop);
      ctx.lineTo(gx, baseY);
      ctx.stroke();
      ctx.setLineDash([]);
    }

    // Draw straight chronological line segments (break on null/missing, no interpolation)
    for (const s of visibleSeries) {
      const strokeColor = colors.palette[s.colorIndex % colors.palette.length];
      ctx.strokeStyle = strokeColor;
      ctx.lineWidth = 2;
      ctx.lineJoin = "round";
      ctx.lineCap = "round";
      ctx.beginPath();

      let inSegment = false;
      for (let i = 0; i < count; i++) {
        const val = s.values[i];
        if (val === null) {
          inSegment = false;
          continue;
        }
        const clamped = Math.max(0, Math.min(1, val));
        const x = pointsX[i];
        const y = padTop + plotH * (1 - clamped);
        if (!inSegment) {
          ctx.moveTo(x, y);
          inSegment = true;
        } else {
          ctx.lineTo(x, y);
        }
      }
      ctx.stroke();
    }

    // Draw point markers for all valid buckets (including isolated single buckets and 0%)
    const baseRadius = count > 36 ? 2.25 : 2.75;
    for (const s of visibleSeries) {
      const fillColor = colors.palette[s.colorIndex % colors.palette.length];
      for (let i = 0; i < count; i++) {
        const val = s.values[i];
        if (val === null) continue;
        const clamped = Math.max(0, Math.min(1, val));
        const x = pointsX[i];
        const y = padTop + plotH * (1 - clamped);
        const isIsolated = (i === 0 || s.values[i - 1] === null) && (i === count - 1 || s.values[i + 1] === null);
        const isActive = i === activeIdx;
        const r = isActive ? 4.5 : (isIsolated ? 3.5 : baseRadius);

        ctx.beginPath();
        ctx.arc(x, y, r, 0, Math.PI * 2);
        ctx.fillStyle = fillColor;
        ctx.fill();
        ctx.lineWidth = isActive ? 1.5 : 1.25;
        ctx.strokeStyle = colors.pointRing;
        ctx.stroke();
      }
    }

    // Non-overlapping Asia/Shanghai X-axis time labels
    const firstBucket = chart.buckets[0];
    ctx.fillStyle = colors.axisText;
    ctx.textAlign = "center";
    ctx.textBaseline = "top";

    const sampleLabel = formatAccessKeyAxisLabel(firstBucket.startMs, chart.range, chart.isDay, spanMs);
    const sampleWidth = Math.max(32, ctx.measureText(sampleLabel).width);
    const minLabelGap = 14;
    const maxLabels = count === 1
      ? 1
      : Math.min(count, Math.max(2, 1 + Math.floor(plotW / (sampleWidth + minLabelGap))));

    const candidateIndices = [];
    for (let k = 0; k < maxLabels; k++) {
      const idx = maxLabels === 1 ? 0 : Math.round((k * (count - 1)) / (maxLabels - 1));
      if (candidateIndices.length === 0 || candidateIndices[candidateIndices.length - 1] !== idx) {
        candidateIndices.push(idx);
      }
    }

    let prevRight = -Infinity;
    for (const idx of candidateIndices) {
      const label = formatAccessKeyAxisLabel(chart.buckets[idx].startMs, chart.range, chart.isDay, spanMs);
      if (!label) continue;
      const w = ctx.measureText(label).width;
      const half = w / 2;
      const minCenter = padLeft + half;
      const maxCenter = cssWidth - padRight - half;
      const cx = maxCenter >= minCenter
        ? Math.max(minCenter, Math.min(maxCenter, pointsX[idx]))
        : padLeft + plotW / 2;
      const left = cx - half;
      const right = cx + half;
      if (left < prevRight + 10) continue;
      ctx.fillText(label, cx, baseY + 8);
      prevRight = right;
    }

    ctx.restore();
  }

  function renderAccessKeyCacheTable() {
    const detailsEl = byId("access-keys-cache-details");
    const thead = byId("access-keys-cache-table-head");
    const tbody = byId("access-keys-cache-table-body");
    if (!detailsEl || !thead || !tbody) return;

    const chart = state.accessKeyCacheChart;
    if (chart.series.length === 0 || chart.buckets.length === 0) {
      detailsEl.hidden = true;
      return;
    }

    detailsEl.hidden = false;
    if (!detailsEl.open) return;
    clearChildren(thead);
    clearChildren(tbody);
    const visibleSeries = getVisibleAccessKeyCacheSeries();

    if (visibleSeries.length === 0) {
      thead.appendChild(
        createEl("tr", null, [
          createEl("th", { attrs: { scope: "col" }, text: "状态" }),
        ])
      );
      tbody.appendChild(
        createEl("tr", null, [
          createEl("td", { className: "text-muted", text: "已隐藏全部 AccessKey 曲线，点击上方图例可重新显示。" }),
        ])
      );
      return;
    }

    const headerCells = [
      createEl("th", { attrs: { scope: "col" }, className: "col-sticky", text: "时间段" }),
    ];
    for (const s of visibleSeries) {
      headerCells.push(
        createEl("th", {
          attrs: { scope: "col", title: s.name },
          className: "num",
          text: s.name,
        })
      );
    }
    thead.appendChild(createEl("tr", null, headerCells));

    for (let i = 0; i < chart.buckets.length; i++) {
      const rowCells = [
        createEl("th", {
          attrs: { scope: "row" },
          className: "col-sticky measured",
          text: formatAccessKeyBucketSpan(chart.buckets[i], chart.range),
        }),
      ];
      for (const s of visibleSeries) {
        const val = s.values[i];
        const hasVal = val !== null;
        rowCells.push(
          createEl("td", {
            className: hasVal ? "num" : "num text-muted",
            text: hasVal ? formatPercent(val) : "无数据",
          })
        );
      }
      tbody.appendChild(createEl("tr", null, rowCells));
    }
  }

  function findAccessKeyChartIndexAtClientX(canvas, clientX) {
    const chart = state.accessKeyCacheChart;
    const pointsX = chart.pointsX;
    if (!pointsX || pointsX.length === 0) return -1;
    const rect = canvas.getBoundingClientRect();
    const x = clientX - rect.left;
    if (x < chart.plotLeft - 12 || x > chart.plotRight + 12) return -1;
    let bestIdx = 0;
    let bestDist = Math.abs(x - pointsX[0]);
    for (let i = 1; i < pointsX.length; i++) {
      const dist = Math.abs(x - pointsX[i]);
      if (dist < bestDist) {
        bestDist = dist;
        bestIdx = i;
      }
    }
    return bestIdx;
  }

  /* ------------------------------------------------------------------------
     API Fetching & State Application (Stale-Proof + Independent Resilience)
     ------------------------------------------------------------------------ */

  function applySectionPayload(targetState, payload, fallbackNowMs) {
    if (!payload || typeof payload !== "object") {
      targetState.error = "返回结构缺失";
      return;
    }
    if (payload.error) {
      targetState.error = String(payload.error);
      return;
    }
    if (payload.data !== undefined && payload.data !== null) {
      targetState.data = payload.data;
      targetState.updatedAtMs = toFiniteNum(payload.updated_at_ms) || fallbackNowMs;
      targetState.error = null;
      return;
    }
    targetState.error = "未返回有效数据";
  }

  function expireCalendarCache(range) {
    if (range !== "today" && range !== "yesterday") return;
    const dayMs = 24 * 60 * 60 * 1000;
    const midnight = Math.floor((Date.now() + BEIJING_OFFSET_MS) / dayMs) * dayMs - BEIJING_OFFSET_MS;
    const start = range === "today" ? midnight : midnight - dayMs;
    for (const cache of [state.usageByRange, state.accessKeysByRange]) {
      const bucket = cache[range];
      if (bucket.data && bucket.data.from_ms !== start) {
        bucket.data = null;
        bucket.updatedAtMs = null;
      }
    }
  }

  function applyAccessKeyPayload(targetState, payload, nowMs) {
    const rows = payload?.data?.items;
    if (Array.isArray(rows) && rows.some(row => row?.error)) {
      const previous = new Map((targetState.data?.items || []).map(row => [row.id, row]));
      payload = {
        ...payload,
        data: {
          ...payload.data,
          items: rows.map(row => {
            const cached = row?.error && previous.get(row.id);
            return cached?.request_count != null ? { ...cached, error: row.error, stale: true } : row;
          }),
        },
      };
    }
    applySectionPayload(targetState, payload, nowMs);
  }

  async function fetchDashboard(options) {
    const opts = options || {};
    const requestedRange = opts.range || state.range;

    if (state.abortController) {
      state.abortController.abort();
    }

    const controller = new AbortController();
    state.abortController = controller;
    const seq = ++state.requestSeq;

    state.isLoading = true;
    state.globalError = null;
    renderTopStatus();

    const url = `/api/dashboard?range=${encodeURIComponent(requestedRange)}`;

    try {
      const response = await fetch(url, {
        method: "GET",
        credentials: "same-origin",
        headers: { Accept: "application/json" },
        signal: controller.signal,
      });

      if (seq !== state.requestSeq) return;

      if (response.status === 401) {
        window.location.assign("/");
        return;
      }

      if (!response.ok) {
        throw new Error(`监控接口返回异常状态（HTTP ${response.status}）`);
      }

      const body = await response.json();
      if (seq !== state.requestSeq) return;

      if (!body || typeof body !== "object") {
        throw new Error("监控接口返回格式无效");
      }

      const nowMs = Date.now();
      applySectionPayload(state.sections.health, body.health, nowMs);
      applySectionPayload(state.sections.concurrency, body.concurrency, nowMs);
      applySectionPayload(state.sections.quota, body.quota, nowMs);

      expireCalendarCache(requestedRange);
      const usageBucket = state.usageByRange[requestedRange];
      if (usageBucket) {
        applySectionPayload(usageBucket, body.usage, nowMs);
      }
      const accessKeysBucket = state.accessKeysByRange[requestedRange];
      if (accessKeysBucket) {
        applyAccessKeyPayload(accessKeysBucket, body.access_keys, nowMs);
      }

      state.lastSyncMs = nowMs;
      state.globalError = null;
    } catch (err) {
      if (err && err.name === "AbortError") {
        return;
      }
      if (seq !== state.requestSeq) return;
      state.globalError = err && err.message
        ? `拉取监控数据失败：${err.message}`
        : "拉取监控数据失败，请稍后重试。";
    } finally {
      if (seq === state.requestSeq) {
        state.isLoading = false;
        state.abortController = null;
        renderAll();
      }
    }
  }

  function renderAll() {
    renderTopStatus();
    renderQuotaSection();
    renderAccessKeysSection();
    renderHealthSection();
    renderUsageSection();
  }

  function scheduleAutoRefresh() {
    if (state.refreshTimerId !== null) {
      window.clearInterval(state.refreshTimerId);
    }
    state.refreshTimerId = window.setInterval(function () {
      if (!document.hidden) {
        fetchDashboard({ range: state.range });
      }
    }, REFRESH_INTERVAL_MS);
  }

  /* ------------------------------------------------------------------------
     Event Wiring
     ------------------------------------------------------------------------ */

  function selectRange(nextRange, pushHistory) {
    if (!VALID_RANGES.has(nextRange)) return;
    const changed = state.range !== nextRange;
    state.range = nextRange;
    syncRangeToUrl(nextRange, pushHistory);
    updateRangeControls();
    if (changed) {
      // Immediately switch UI to target range's own caches (never showing other range mislabeled)
      renderAccessKeysSection();
      renderUsageSection();
    }
    scheduleAutoRefresh();
    fetchDashboard({ range: nextRange });
  }

  function selectRankTab(tabKey) {
    state.activeRankTab = tabKey;
    const tabs = document.querySelectorAll("[data-rank-tab]");
    for (const tab of tabs) {
      const isSelected = tab.dataset.rankTab === tabKey;
      tab.setAttribute("aria-selected", String(isSelected));
      const panelId = tab.getAttribute("aria-controls");
      const panel = panelId ? byId(panelId) : null;
      if (panel) panel.hidden = !isSelected;
    }
  }

  function wireEvents() {
    const rangeButtons = document.querySelectorAll("[data-range]");
    const refreshBtn = byId("refresh-btn");
    const accessKeyTable = byId("access-keys-table");
    accessKeyTable.tHead.addEventListener("focusin", event => {
      const stickyRight = accessKeyTable.tHead.rows[0].cells[0].getBoundingClientRect().right;
      const overlap = stickyRight - event.target.getBoundingClientRect().left;
      if (overlap > 0) byId("access-keys-scroll").scrollLeft -= overlap;
    });
    if ("commandForElement" in HTMLButtonElement.prototype) {
      accessKeyTable.addEventListener("command", event => {
        if (event.command === "--sort") setAccessKeySort(event.source);
      });
    } else {
      for (const button of accessKeyTable.tHead.querySelectorAll('button[command="--sort"]')) {
        button.addEventListener("click", () => setAccessKeySort(button));
      }
    }

    for (const btn of rangeButtons) {
      btn.addEventListener("click", function () {
        selectRange(btn.dataset.range, true);
      });
    }
    if (refreshBtn) {
      refreshBtn.addEventListener("click", function () {
        scheduleAutoRefresh();
        fetchDashboard({ range: state.range });
      });
    }

    // Ranking tabs
    const rankTabs = document.querySelectorAll("[data-rank-tab]");
    for (const tab of rankTabs) {
      tab.addEventListener("click", function () {
        selectRankTab(tab.dataset.rankTab);
      });
    }

    // Ranking expand/collapse buttons
    const expandBtns = document.querySelectorAll("[data-rank-expand]");
    for (const btn of expandBtns) {
      btn.addEventListener("click", function () {
        const key = btn.dataset.rankExpand;
        if (!key) return;
        state.expandedRankTabs[key] = !state.expandedRankTabs[key];
        const activeUsage = state.usageByRange[state.range];
        const rankings = activeUsage.data && activeUsage.data.rankings ? activeUsage.data.rankings : null;
        renderRankings(rankings, Boolean(activeUsage.data), Boolean(activeUsage.error));
      });
    }

    // Chart pointer & keyboard inspection
    const canvas = byId("usage-trend-canvas");
    if (canvas) {
      canvas.addEventListener("pointermove", function (ev) {
        const idx = findChartIndexAtClientX(canvas, ev.clientX);
        if (idx !== state.chart.hoveredIndex) {
          state.chart.hoveredIndex = idx;
          renderTrendChart();
        }
      });

      canvas.addEventListener("pointerleave", function () {
        if (state.chart.hoveredIndex !== -1) {
          state.chart.hoveredIndex = -1;
          renderTrendChart();
        }
      });

      canvas.addEventListener("keydown", function (ev) {
        const len = state.chart.series.length;
        if (len === 0) return;
        if (ev.key === "ArrowRight") {
          ev.preventDefault();
          state.chart.focusedIndex = state.chart.focusedIndex < 0 ? 0 : Math.min(len - 1, state.chart.focusedIndex + 1);
          renderTrendChart();
        } else if (ev.key === "ArrowLeft") {
          ev.preventDefault();
          state.chart.focusedIndex = state.chart.focusedIndex < 0 ? len - 1 : Math.max(0, state.chart.focusedIndex - 1);
          renderTrendChart();
        } else if (ev.key === "Home") {
          ev.preventDefault();
          state.chart.focusedIndex = 0;
          renderTrendChart();
        } else if (ev.key === "End") {
          ev.preventDefault();
          state.chart.focusedIndex = len - 1;
          renderTrendChart();
        } else if (ev.key === "Escape") {
          state.chart.focusedIndex = -1;
          renderTrendChart();
        }
      });

      canvas.addEventListener("blur", function () {
        if (state.chart.focusedIndex !== -1) {
          state.chart.focusedIndex = -1;
          renderTrendChart();
        }
      });

      if (typeof ResizeObserver === "function") {
        const ro = new ResizeObserver(function () {
          renderTrendChart();
        });
        ro.observe(canvas);
      } else {
        window.addEventListener("resize", renderTrendChart);
      }
    }

    // AccessKey cache-hit chart legend toggles, pointer/tap & keyboard inspection
    const akLegend = byId("access-keys-cache-legend");
    if (akLegend) {
      akLegend.addEventListener("click", function (ev) {
        const btn = ev.target && typeof ev.target.closest === "function"
          ? ev.target.closest("button[data-cache-key-id]")
          : null;
        if (!btn) return;
        const keyId = btn.dataset.cacheKeyId;
        if (!keyId) return;
        const chart = state.accessKeyCacheChart;
        if (chart.hiddenKeyIds.has(keyId)) {
          chart.hiddenKeyIds.delete(keyId);
          btn.setAttribute("aria-pressed", "true");
        } else {
          chart.hiddenKeyIds.add(keyId);
          btn.setAttribute("aria-pressed", "false");
        }
        const visibleAfter = getVisibleAccessKeyCacheSeries();
        if (visibleAfter.length === 0) {
          chart.hoveredIndex = -1;
          chart.focusedIndex = -1;
          chart.interactionSource = null;
          renderAccessKeyCacheChart(true);
        } else {
          renderAccessKeyCacheChart(false);
        }
        renderAccessKeyCacheTable();
      });
    }

    const akDetails = byId("access-keys-cache-details");
    if (akDetails) akDetails.addEventListener("toggle", renderAccessKeyCacheTable);

    const akCanvas = byId("access-keys-cache-canvas");
    if (akCanvas) {
      akCanvas.addEventListener("pointermove", function (ev) {
        if (ev.pointerType === "touch") return;
        const idx = findAccessKeyChartIndexAtClientX(akCanvas, ev.clientX);
        if (idx !== state.accessKeyCacheChart.hoveredIndex) {
          state.accessKeyCacheChart.hoveredIndex = idx;
          renderAccessKeyCacheChart(false);
        }
      });

      akCanvas.addEventListener("pointerleave", function (ev) {
        if (ev.pointerType === "touch") return;
        if (state.accessKeyCacheChart.hoveredIndex !== -1) {
          state.accessKeyCacheChart.hoveredIndex = -1;
          renderAccessKeyCacheChart(false);
        }
      });

      akCanvas.addEventListener("pointerdown", function (ev) {
        if (ev.pointerType === "mouse" && ev.button !== 0) return;
        const idx = findAccessKeyChartIndexAtClientX(akCanvas, ev.clientX);
        if (idx < 0) return;
        const chart = state.accessKeyCacheChart;
        chart.focusedIndex = idx;
        chart.interactionSource = "tap";
        if (ev.pointerType !== "mouse") {
          chart.hoveredIndex = -1;
        }
        renderAccessKeyCacheChart(true);
      });

      akCanvas.addEventListener("keydown", function (ev) {
        const chart = state.accessKeyCacheChart;
        const len = chart.buckets.length;
        if (len === 0 || getVisibleAccessKeyCacheSeries().length === 0) return;
        if (ev.key === "ArrowRight") {
          ev.preventDefault();
          chart.hoveredIndex = -1;
          chart.focusedIndex = chart.focusedIndex < 0 ? 0 : Math.min(len - 1, chart.focusedIndex + 1);
          chart.interactionSource = "keyboard";
          renderAccessKeyCacheChart(true);
        } else if (ev.key === "ArrowLeft") {
          ev.preventDefault();
          chart.hoveredIndex = -1;
          chart.focusedIndex = chart.focusedIndex < 0 ? len - 1 : Math.max(0, chart.focusedIndex - 1);
          chart.interactionSource = "keyboard";
          renderAccessKeyCacheChart(true);
        } else if (ev.key === "Home") {
          ev.preventDefault();
          chart.hoveredIndex = -1;
          chart.focusedIndex = 0;
          chart.interactionSource = "keyboard";
          renderAccessKeyCacheChart(true);
        } else if (ev.key === "End") {
          ev.preventDefault();
          chart.hoveredIndex = -1;
          chart.focusedIndex = len - 1;
          chart.interactionSource = "keyboard";
          renderAccessKeyCacheChart(true);
        } else if (ev.key === "Escape") {
          if (chart.focusedIndex !== -1) {
            chart.focusedIndex = -1;
            chart.interactionSource = null;
            renderAccessKeyCacheChart(false);
          }
        }
      });

      akCanvas.addEventListener("blur", function () {
        const chart = state.accessKeyCacheChart;
        if (chart.interactionSource === "keyboard" && chart.focusedIndex !== -1) {
          chart.focusedIndex = -1;
          chart.interactionSource = null;
          renderAccessKeyCacheChart(false);
        }
      });

      if (typeof ResizeObserver === "function") {
        const akRo = new ResizeObserver(function () {
          renderAccessKeyCacheChart(false);
        });
        akRo.observe(akCanvas);
      } else {
        window.addEventListener("resize", function () {
          renderAccessKeyCacheChart(false);
        });
      }
    }

    // Browser history back/forward
    window.addEventListener("popstate", function () {
      const urlRange = readRangeFromUrl();
      if (urlRange !== state.range) {
        state.range = urlRange;
        updateRangeControls();
        renderAccessKeysSection();
        renderUsageSection();
        fetchDashboard({ range: urlRange });
      }
    });

    // Refresh promptly if tab returns to visible after interval elapsed
    document.addEventListener("visibilitychange", function () {
      if (!document.hidden && state.lastSyncMs && (Date.now() - state.lastSyncMs) >= REFRESH_INTERVAL_MS) {
        fetchDashboard({ range: state.range });
      }
    });
  }

  /* ------------------------------------------------------------------------
     Bootstrap
     ------------------------------------------------------------------------ */

  function init() {
    state.range = readRangeFromUrl();
    syncRangeToUrl(state.range, false);
    updateRangeControls();
    wireEvents();
    renderAll();

    state.countdownTimerId = window.setInterval(tickCountdowns, 1000);
    scheduleAutoRefresh();
    fetchDashboard({ range: state.range });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
