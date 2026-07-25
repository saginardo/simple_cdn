import { Monitor, Moon, Sun } from "lucide-react";
import { useTheme } from "next-themes";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { t, useI18n } from "@/lib/i18n";

const modes = {
  light: { label: "浅色", icon: Sun },
  dark: { label: "深色", icon: Moon },
  system: { label: "跟随系统", icon: Monitor },
} as const;

type ThemeMode = keyof typeof modes;

export function ThemeToggle() {
  useI18n();
  const { theme, setTheme } = useTheme();
  const mode: ThemeMode =
    theme && theme in modes ? (theme as ThemeMode) : "system";
  const CurrentIcon = modes[mode].icon;

  return (
    <DropdownMenu>
      <Tooltip>
        <TooltipTrigger asChild>
          <DropdownMenuTrigger asChild>
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={t("主题：{mode}", {
                mode: t(modes[mode].label),
              })}
            >
              <CurrentIcon />
            </Button>
          </DropdownMenuTrigger>
        </TooltipTrigger>
        <TooltipContent>{t("切换主题")}</TooltipContent>
      </Tooltip>
      <DropdownMenuContent align="end" className="min-w-36">
        <DropdownMenuRadioGroup
          value={mode}
          onValueChange={(value) => {
            if (value in modes) setTheme(value);
          }}
        >
          {(Object.keys(modes) as ThemeMode[]).map((key) => {
            const Icon = modes[key].icon;
            return (
              <DropdownMenuRadioItem key={key} value={key}>
                <Icon />
                {t(modes[key].label)}
              </DropdownMenuRadioItem>
            );
          })}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
