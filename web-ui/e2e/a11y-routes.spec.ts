/**
 * Every page of the console, signed in and signed out, in light and dark,
 * at phone and desktop width, has no axe violation of any impact: WCAG 2.0
 * to 2.2 A and AA and axe's best practices. The list of pages comes from
 * src/routes, so a new page is checked from its first commit: a page that
 * is neither listed below nor explained in SKIPPED fails the first test.
 *
 * Dialogs, menus and other states are checked by the specs of their pages
 * (expectNoAxeViolations in e2e/*.spec.ts).
 */
import AxeBuilder from '@axe-core/playwright';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import type { Page } from '@playwright/test';
import { test, expect } from './fixtures.ts';
import { UI } from './roles.ts';

/** Pages and the address each is checked at (dynamic segments filled in) */
const CONSOLE: Record<string, string> = {
	'/(console)': '/',
	'/(console)/account': '/account',
	'/(console)/activity': '/activity',
	'/(console)/attestation': '/attestation',
	'/(console)/authorize-device/[id]': '/authorize-device/no-such-request',
	'/(console)/automation': '/automation',
	'/(console)/billing': '/billing',
	'/(console)/cdap': '/cdap',
	'/(console)/cdap/devices': '/cdap/devices',
	'/(console)/chat': '/chat',
	'/(console)/cross-platform': '/cross-platform',
	'/(console)/dataguard': '/dataguard',
	'/(console)/devices': '/devices',
	'/(console)/devices/add': '/devices/add',
	'/(console)/fleet': '/fleet',
	'/(console)/help-requests': '/help-requests',
	'/(console)/help-requests/feedback': '/help-requests/feedback',
	'/(console)/inventory': '/inventory',
	'/(console)/languages': '/languages',
	'/(console)/network': '/network',
	'/(console)/organizations': '/organizations',
	'/(console)/permissions': '/permissions',
	'/(console)/policies/[[orgId]]': '/policies',
	'/(console)/registrations': '/registrations',
	'/(console)/remote/sessions': '/remote/sessions',
	'/(console)/reports': '/reports',
	'/(console)/resource-control': '/resource-control',
	'/(console)/scaling': '/scaling',
	'/(console)/security-audit': '/security-audit',
	'/(console)/security-checks': '/security-checks',
	'/(console)/server-attestation': '/server-attestation',
	'/(console)/server-management': '/server-management',
	'/(console)/server/backups': '/server/backups',
	'/(console)/server/config-files': '/server/config-files',
	'/(console)/server/updates': '/server/updates',
	'/(console)/settings': '/settings',
	'/(console)/sites': '/sites',
	'/(console)/tickets': '/tickets',
	'/(console)/toolkit': '/toolkit',
	'/(console)/users': '/users',
	'/(console)/users/[id]': '/users/1',
	'/(studio)/cdap-studio': '/cdap-studio',
	'/(remote)/remote': '/remote'
};
const SETTINGS_TABS = ['auth', 'ldap', 'sso', 'joining', 'branding', 'brand', 'email', 'time', 'meshcentral'];

const PUBLIC: Record<string, string> = {
	'/(public)/login': '/login?autoLaunch=0',
	'/(public)/portal': '/portal',
	'/(public)/confirm-email': '/confirm-email',
	'/(public)/invite': '/invite',
	'/(public)/set-password': '/set-password',
	'/(public)/join/[code]': '/join/AAAA-BBBB',
	'/(public)/s/[code]': '/s/AAAA-BBBB',
	'/(public)/d/[publicId]': '/d/unknown',
	'/(public)/error/[code]': '/error/404',
	'/(remote)/remote/login': '/remote/login',
	'/(remote)/remote/guest': '/remote/guest'
};

/** Pages this test leaves to others, and why */
const SKIPPED: Record<string, string> = {
	'/(console)/kit': 'developer page, 404 in a build',
	'/(console)/cdap/devices/[id]': 'needs a CDAP device; devices-area-cdap.spec.ts checks it with axe',
	'/(console)/organizations/[id]': 'needs an organization id; people.spec.ts checks it with axe',
	'/(console)/remote/session/[deviceId]':
		'needs a live remote computer; remote.spec.ts checks the viewer with axe',
	'/(remote)/remote/[deviceId]': 'needs a live remote computer; remote.spec.ts checks the viewer with axe'
};

const THEMES = ['light', 'dark'] as const;
const WIDTHS = [320, 1280] as const;

function routeIds(): string[] {
	const root = fileURLToPath(new URL('../src/routes', import.meta.url));
	const out: string[] = [];
	const walk = (dir: string) => {
		for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
			const p = path.join(dir, e.name);
			if (e.isDirectory()) walk(p);
			else if (e.name === '+page.svelte') out.push('/' + path.relative(root, dir).split(path.sep).join('/'));
		}
	};
	walk(root);
	return out.map((id) => (id === '/' ? '/' : id.replace(/\/$/, ''))).sort();
}

async function violations(page: Page): Promise<string[]> {
	await page.waitForLoadState('networkidle', { timeout: 15_000 }).catch(() => undefined);
	await page.evaluate(async () => {
		const ending = document
			.getAnimations()
			.filter((a) => a.playState === 'running' && a.effect?.getComputedTiming().iterations !== Infinity)
			.map((a) => a.finished.catch(() => undefined));
		await Promise.race([Promise.all(ending), new Promise((done) => setTimeout(done, 2_000))]);
	});
	const r = await new AxeBuilder({ page })
		// A frame sandboxed without scripts (the mail preview) can't run axe; entering it stalls the run
		.exclude('iframe[sandbox]')
		.withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'])
		.analyze();
	return r.violations.map(
		(v) => `${v.id} (${v.impact}): ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`
	);
}

async function eachLook(page: Page, address: string): Promise<string[]> {
	const found: string[] = [];
	for (const theme of THEMES) {
		for (const width of WIDTHS) {
			await page.setViewportSize({ width, height: width === 320 ? 640 : 900 });
			// The console's default theme follows the system's
			await page.emulateMedia({ colorScheme: theme });
			await page.goto(`${UI}${address}`);
			await expect(page.locator('h1').first()).toBeVisible({ timeout: 15_000 });
			for (const v of await violations(page)) found.push(`${theme} ${width}px: ${v}`);
		}
	}
	return found;
}

test('every page is checked here or explained', () => {
	const known = new Set([...Object.keys(CONSOLE), ...Object.keys(PUBLIC), ...Object.keys(SKIPPED)]);
	expect(routeIds().filter((id) => !known.has(id))).toEqual([]);
});

test.describe('signed in', () => {
	test.describe.configure({ timeout: 180_000 });
	const addresses = [...Object.values(CONSOLE), ...SETTINGS_TABS.map((t) => `/settings?tab=${t}`)];
	for (const address of addresses) {
		test(`axe: ${address}`, async ({ page }) => {
			expect(await eachLook(page, address)).toEqual([]);
		});
	}
});

test.describe('signed out', () => {
	test.describe.configure({ timeout: 180_000 });
	// Unknown codes and links answer 404, a guest page without its token 400, and there is no session to ask for
	test.use({ storageState: { cookies: [], origins: [] }, allowProblems: [/(http|status of) 40[014]/] });
	for (const address of Object.values(PUBLIC)) {
		test(`axe: ${address}`, async ({ page }) => {
			expect(await eachLook(page, address)).toEqual([]);
		});
	}
});
