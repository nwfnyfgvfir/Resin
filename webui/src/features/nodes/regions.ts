import * as countries from "i18n-iso-countries";
import enLocale from "i18n-iso-countries/langs/en.json";
import zhLocale from "i18n-iso-countries/langs/zh.json";
import { getCurrentLocale, isEnglishLocale } from "../../i18n/locale";

countries.registerLocale(enLocale);
countries.registerLocale(zhLocale);

export interface RegionOption {
    code: string;
    name: string;
    searchText: string;
}

function getCountryLocale(): "zh" | "en" {
    return isEnglishLocale(getCurrentLocale()) ? "en" : "zh";
}

export const getAllRegions = (locale = getCurrentLocale()): RegionOption[] => {
    const names = countries.getNames(isEnglishLocale(locale) ? "en" : "zh", { select: "official" });
    return Object.entries(names).map(([code, name]) => ({
        code,
        name: `${code} (${name})`,
        searchText: `${code} ${countries.getName(code, "zh")} ${countries.getName(code, "en")}`.toLocaleLowerCase(),
    })).sort((a, b) => a.code.localeCompare(b.code));
};

export const getRegionName = (code: string): string | undefined => {
    return countries.getName(code, getCountryLocale(), { select: "official" });
};
