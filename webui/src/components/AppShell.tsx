import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import {
  AlertTriangle,
  Cable,
  Database,
  LayoutDashboard,
  LogOut,
  Logs,
  Menu,
  Network,
  Regex,
  Rss,
  Server,
  Settings,
  X,
} from "lucide-react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { motion } from "framer-motion";
import { Button } from "./ui/Button";
import { cn } from "../lib/cn";
import { apiRequest } from "../lib/api-client";
import { useAuthStore } from "../features/auth/auth-store";
import { getEnvConfig } from "../features/systemConfig/api";
import { useI18n } from "../i18n";
import { LanguageSwitcher } from "./LanguageSwitcher";

type NavItem = {
  label: string;
  path: string;
  icon: typeof LayoutDashboard;
};

const navItems: NavItem[] = [
  { label: "总览看板", path: "/dashboard", icon: LayoutDashboard },
  { label: "平台管理", path: "/platforms", icon: Server },
  { label: "订阅管理", path: "/subscriptions", icon: Rss },
  { label: "节点池", path: "/nodes", icon: Network },
  { label: "接入点", path: "/endpoints", icon: Cable },
  { label: "请求头规则", path: "/rules", icon: Regex },
  { label: "请求日志", path: "/request-logs", icon: Logs },
  { label: "资源", path: "/resources", icon: Database },
  { label: "系统配置", path: "/system-config", icon: Settings },
];

type SystemInfoResponse = {
  version: string;
};

export function AppShell() {
  const { t } = useI18n();
  const clearToken = useAuthStore((state) => state.clearToken);
  const token = useAuthStore((state) => state.token);
  const navigate = useNavigate();
  const { pathname } = useLocation();
  const menu = useRef<HTMLDialogElement>(null);
  const [menuOpen, setMenuOpen] = useState(false);
  const envConfigQuery = useQuery({
    queryKey: ["system-config-env", "shell"],
    queryFn: getEnvConfig,
    staleTime: 30_000,
  });
  const systemInfoQuery = useQuery({
    queryKey: ["system-info", "shell"],
    queryFn: () => apiRequest<SystemInfoResponse>("/api/v1/system/info"),
    staleTime: 300_000,
  });
  const logoSrc = `${import.meta.env.BASE_URL}vite.svg`;
  const version = systemInfoQuery.data?.version?.trim();
  const currentPage = navItems.find(({ path }) => pathname === path || pathname.startsWith(`${path}/`));

  useEffect(() => {
    const desktop = window.matchMedia("(width > 1120px)");
    const closeOnDesktop = () => {
      if (desktop.matches) menu.current?.close();
    };
    desktop.addEventListener("change", closeOnDesktop);
    return () => desktop.removeEventListener("change", closeOnDesktop);
  }, []);

  useEffect(() => {
    menu.current?.close();
  }, [pathname]);

  const envConfig = envConfigQuery.data;
  const persistenceDialect = envConfig?.persistence_dialect?.trim();
  const persistenceLabel = persistenceDialect
    ? persistenceDialect.toLowerCase() === "postgres"
      ? "PostgreSQL"
      : persistenceDialect.toLowerCase() === "sqlite"
        ? "SQLite"
        : persistenceDialect
    : null;
  const authWarnings: string[] = [];
  if (envConfig && !envConfig.admin_token_set) {
    authWarnings.push(t("RESIN_ADMIN_TOKEN 为空，控制面 API 免认证"));
  }
  if (envConfig && !envConfig.proxy_token_set) {
    authWarnings.push(t("RESIN_PROXY_TOKEN 为空，正/反向代理免认证"));
  }
  if (envConfig && envConfig.admin_token_set && envConfig.admin_token_weak) {
    authWarnings.push(t("RESIN_ADMIN_TOKEN 强度较弱，建议更换为更高熵随机令牌"));
  }
  if (envConfig && envConfig.proxy_token_set && envConfig.proxy_token_weak) {
    authWarnings.push(t("RESIN_PROXY_TOKEN 强度较弱，建议更换为更高熵随机令牌"));
  }
  const showAuthWarning = authWarnings.length > 0;

  const logout = () => {
    menu.current?.close();
    clearToken();
    navigate("/login", { replace: true });
  };

  const closeMenuAfterNavigation = () => {
    if (menu.current?.open) {
      menu.current.close();
      window.scrollTo(0, 0);
    }
  };

  const brand = (
    <div className="brand">
      <div className="brand-logo" aria-hidden="true">
        <img src={logoSrc} alt="" />
      </div>
      <div className="brand-copy">
        <div className="brand-title-row">
          <p className="brand-title">Resin</p>
          {version ? (
            <span className="brand-version" title={version} aria-label={`Version ${version}`}>
              {version}
            </span>
          ) : null}
        </div>
        <p className="brand-subtitle">{t("高性能粘性代理池 · 管理面板")}</p>
        {persistenceLabel ? <p className="brand-meta">{t("数据库类型")}: {persistenceLabel}</p> : null}
      </div>
    </div>
  );

  const navigation = (
    <>
      <div className="sidebar-main">
        <nav className="nav-list" aria-label={t("主导航")}>
          {navItems.map((item) => {
            const Icon = item.icon;
            return (
              <NavLink
                key={item.path}
                to={item.path}
                onClick={closeMenuAfterNavigation}
                className={({ isActive }) => cn("nav-item", isActive && "nav-item-active")}
              >
                <Icon size={16} />
                <span>{t(item.label)}</span>
              </NavLink>
            );
          })}
        </nav>
      </div>

      <div className="sidebar-bottom">
        {showAuthWarning ? (
          <div className="callout callout-warning sidebar-warning" role="alert">
            <AlertTriangle size={16} />
            <div className="sidebar-warning-copy">
              <strong>{t("安全警告")}</strong>
              <div className="sidebar-warning-list">
                {authWarnings.map((warning) => (
                  <span key={warning}>{warning}</span>
                ))}
              </div>
            </div>
          </div>
        ) : null}

        {!token ? <p className="sidebar-hint">{t("当前为免认证访问模式")}</p> : null}

        <div className="sidebar-tools">
          {token ? (
            <Button
              variant="secondary"
              size="sm"
              className="sidebar-icon-btn"
              onClick={logout}
              aria-label={t("退出登录")}
              title={t("退出登录")}
            >
              <LogOut size={16} />
            </Button>
          ) : (
            <span className="sidebar-tool-spacer" aria-hidden="true" />
          )}
          <LanguageSwitcher className="sidebar-locale" compact />
        </div>
      </div>
    </>
  );

  return (
    <div className="app-layout">
      <aside className="sidebar">
        {brand}
        {navigation}
      </aside>

      <header className="mobile-header">
        <div className="mobile-header-title">
          <div className="brand-logo" aria-hidden="true"><img src={logoSrc} alt="" /></div>
          <div className="mobile-header-copy">
            <span>Resin</span>
            {currentPage ? <strong>{t(currentPage.label)}</strong> : null}
          </div>
        </div>
        <Button
          variant="secondary"
          size="sm"
          aria-controls="mobile-navigation"
          aria-expanded={menuOpen}
          aria-haspopup="dialog"
          title={showAuthWarning ? t("安全警告") : undefined}
          onClick={() => {
            menu.current?.showModal();
            setMenuOpen(true);
          }}
        >
          {showAuthWarning ? <AlertTriangle size={18} className="mobile-menu-warning" /> : <Menu size={18} />}
          {t("菜单")}
        </Button>
      </header>

      <dialog
        ref={menu}
        id="mobile-navigation"
        className="mobile-menu"
        aria-label={t("主导航")}
        onClose={() => setMenuOpen(false)}
        onClick={(event) => {
          if (event.target === event.currentTarget) event.currentTarget.close();
        }}
      >
        <div className="mobile-menu-content">
          <div className="mobile-menu-header">
            {brand}
            <Button variant="ghost" size="sm" aria-label={t("关闭")} onClick={() => menu.current?.close()}>
              <X size={18} />
            </Button>
          </div>
          {navigation}
        </div>
      </dialog>

      <main className="main">
        <motion.div
          key="content"
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          transition={{ duration: 0.24, ease: "easeOut" }}
          className="content"
        >
          <Outlet />
        </motion.div>
      </main>
    </div>
  );
}
