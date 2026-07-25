import { getLocale, t, type Locale } from "@/lib/i18n";
const numberFormatters = new Map<Locale, Intl.NumberFormat>();
const compactFormatters = new Map<Locale, Intl.NumberFormat>();
function numberFormatter() {
  const locale = getLocale();
  const existing = numberFormatters.get(locale);
  if (existing) return existing;
  const formatter = new Intl.NumberFormat(locale);
  numberFormatters.set(locale, formatter);
  return formatter;
}
function compactFormatter() {
  const locale = getLocale();
  const existing = compactFormatters.get(locale);
  if (existing) return existing;
  const formatter = new Intl.NumberFormat(locale, {
    notation: "compact",
    maximumFractionDigits: 2,
  });
  compactFormatters.set(locale, formatter);
  return formatter;
}
export function formatNumber(value: number | null | undefined) {
  return numberFormatter().format(Number(value ?? 0));
}
export function formatCompact(value: number | null | undefined) {
  return compactFormatter().format(Number(value ?? 0));
}
export function formatBytes(value: number | null | undefined) {
  const bytes = Math.max(0, Number(value ?? 0));
  if (!bytes) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  const index = Math.min(
    Math.floor(Math.log(bytes) / Math.log(1024)),
    units.length - 1,
  );
  return `${(bytes / 1024 ** index).toLocaleString(getLocale(), {
    minimumFractionDigits: index ? 1 : 0,
    maximumFractionDigits: index ? 1 : 0,
  })} ${units[index]}`;
}
export function formatPercent(
  value: number | null | undefined,
  fractionDigits = 1,
) {
  return Number(value ?? 0).toLocaleString(getLocale(), {
    style: "percent",
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: fractionDigits,
  });
}
export function formatDateTime(value: string | null | undefined) {
  if (!value) return "--";
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? "--"
    : date.toLocaleString(getLocale(), {
        hour12: false,
      });
}
export function formatDate(value: string | null | undefined) {
  if (!value) return "--";
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? "--"
    : date.toLocaleDateString(getLocale());
}
export function formatDuration(seconds: number | null | undefined) {
  let remaining = Math.max(0, Math.floor(Number(seconds ?? 0)));
  const days = Math.floor(remaining / 86400);
  remaining %= 86400;
  const hours = Math.floor(remaining / 3600);
  remaining %= 3600;
  const minutes = Math.floor(remaining / 60);
  const parts = [];
  if (days)
    parts.push(
      t("{value0} 天", {
        value0: days,
      }),
    );
  if (hours)
    parts.push(
      t("{value0} 小时", {
        value0: hours,
      }),
    );
  if (minutes || !parts.length)
    parts.push(
      t("{value0} 分钟", {
        value0: minutes,
      }),
    );
  return parts.join(" ");
}
export function shortHash(value: string | null | undefined) {
  return value ? `${value.slice(0, 10)}...${value.slice(-6)}` : "--";
}
