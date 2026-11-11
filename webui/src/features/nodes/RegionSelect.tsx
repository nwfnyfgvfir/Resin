import { ChevronDown, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState, type CSSProperties } from "react";
import { Input } from "../../components/ui/Input";
import { useI18n } from "../../i18n";
import { getAllRegions } from "./regions";

type Props = {
  id: string;
  value: string;
  onChange: (code: string) => void;
  style?: CSSProperties;
};

export function RegionSelect({ id, value, onChange, style }: Props) {
  const { t, locale } = useI18n();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const root = useRef<HTMLDivElement>(null);
  const list = useRef<HTMLDivElement>(null);
  const regions = useMemo(() => getAllRegions(locale), [locale]);
  const matches = regions.filter((region) => region.searchText.includes(search.trim().toLocaleLowerCase()));
  const options = search.trim() ? matches : [{ code: "", name: t("全部"), searchText: "" }, ...matches];
  const selectedName = regions.find((region) => region.code === value)?.name ?? value;
  const currentIndex = Math.max(0, Math.min(activeIndex, options.length - 1));

  useEffect(() => {
    if (!open) return;
    const closeOutside = (event: MouseEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", closeOutside);
    return () => document.removeEventListener("mousedown", closeOutside);
  }, [open]);

  useEffect(() => {
    if (open) list.current?.children[currentIndex]?.scrollIntoView({ block: "nearest" });
  }, [open, currentIndex, search]);

  const choose = (code: string) => {
    onChange(code);
    setOpen(false);
    setSearch("");
  };

  return (
    <div className="region-combobox" ref={root} onBlur={(event) => {
      if (!event.currentTarget.contains(event.relatedTarget as Node | null)) setOpen(false);
    }}>
      <Input
        id={id}
        role="combobox"
        autoComplete="off"
        aria-expanded={open}
        aria-autocomplete="list"
        aria-controls={`${id}-options`}
        aria-activedescendant={open && options.length ? `${id}-option-${currentIndex}` : undefined}
        placeholder={open ? t("搜索区域名称或代码") : t("全部")}
        value={open ? search : selectedName}
        style={{ ...style, paddingRight: "2rem" }}
        onFocus={() => { setOpen(true); setSearch(""); setActiveIndex(0); }}
        onClick={() => { if (!open) { setOpen(true); setSearch(""); setActiveIndex(0); } }}
        onChange={(event) => { setSearch(event.target.value); setActiveIndex(0); setOpen(true); }}
        onKeyDown={(event) => {
          if (event.key === "Escape") {
            event.preventDefault();
            setOpen(false);
          } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
            event.preventDefault();
            if (!open) { setOpen(true); setSearch(""); setActiveIndex(0); return; }
            if (options.length) setActiveIndex((current) => (current + (event.key === "ArrowDown" ? 1 : -1) + options.length) % options.length);
          } else if (event.key === "Enter" && open && options.length) {
            event.preventDefault();
            choose(options[currentIndex].code);
          }
        }}
      />
      {value ? (
        <button type="button" className="region-combobox-icon" aria-label={t("清除区域筛选")}
          onMouseDown={(event) => event.preventDefault()} onClick={() => choose("")}><X size={14} /></button>
      ) : <ChevronDown size={14} className="region-combobox-icon" aria-hidden="true" />}
      {open && (
        <div className="region-combobox-menu" id={`${id}-options`} role="listbox" aria-label={t("区域")} ref={list}>
          {options.map((option, index) => (
            <div key={option.code} id={`${id}-option-${index}`} role="option"
              aria-selected={option.code === value}
              className={index === currentIndex ? "region-combobox-option active" : "region-combobox-option"}
              onMouseDown={(event) => event.preventDefault()}
              onMouseEnter={() => setActiveIndex(index)}
              onClick={() => choose(option.code)}>{option.name}</div>
          ))}
          {!matches.length && <p className="muted region-combobox-empty">{t("没有匹配的区域")}</p>}
        </div>
      )}
    </div>
  );
}
