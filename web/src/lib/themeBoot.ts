/**
 * The theme rule shared by the head script and `useTheme`: the person's stored
 * choice on this device, else the system's scheme. Kept in a module with no
 * client directive so the root layout (a server component) can inline the
 * script's text, and kept beside the hook's constants so the two cannot drift.
 * Online follows the same rule.
 */

export const THEME_STORAGE_KEY = 'peasant-theme';

export const LIGHT_SCHEME = '(prefers-color-scheme: light)';

/**
 * Runs as a plain inline script in the document head, while the HTML is still
 * being parsed: it sets both theme attributes (the app's and the transcript
 * package's) before the body exists, so the first frame is already in the
 * right theme.
 */
export const THEME_BOOT_SCRIPT = `(function(){try{var t=localStorage.getItem(${JSON.stringify(THEME_STORAGE_KEY)});if(t!=="light"&&t!=="dark"){t=window.matchMedia&&window.matchMedia(${JSON.stringify(LIGHT_SCHEME)}).matches?"light":"dark"}var d=document.documentElement;d.setAttribute("data-theme",t);d.setAttribute("data-tb-theme",t)}catch(e){}})();`;
