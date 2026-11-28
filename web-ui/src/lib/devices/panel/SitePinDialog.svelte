<!--
  Pin a device to a site, or take its pin away so its folder or its
  address decide again. One question, so a small centred dialog. The sites
  are the ones the account sees; the server refuses a site of another
  organization than the device's and says so.
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import { listSites, setDeviceSite, type Site } from '#lib/api/sites.ts';
	import { t } from '#lib/i18n/i18n.svelte.ts';
	import Button from '#lib/ui/Button.svelte';
	import Dialog from '#lib/ui/Dialog.svelte';
	import Field from '#lib/ui/Field.svelte';
	import FormStatus from '#lib/ui/FormStatus.svelte';
	import Select from '#lib/ui/Select.svelte';
	import States from '#lib/ui/States.svelte';
	import { createForm } from '#lib/ui/form.svelte.ts';
	import { devicesChanged } from '../actions.svelte.ts';
	import { say } from '../panel.svelte.ts';

	interface Props {
		deviceId: string;
		deviceName: string;
		/** The site the device is pinned to now, "" for none */
		pinned: string;
		onclose: () => void;
	}

	let { deviceId, deviceName, pinned, onclose }: Props = $props();

	let sites = $state.raw<Site[] | null>(null);
	let loadError = $state<string | null>(null);

	async function load() {
		loadError = null;
		try {
			sites = (await listSites()).sites;
		} catch (err) {
			loadError = err instanceof Error && err.message ? err.message : t('sites.load_failed');
		}
	}
	$effect(() => {
		untrack(() => void load());
	});

	const options = $derived([
		{ value: '', label: t('sites.pin_none') },
		...(sites ?? []).map((s) => ({ value: s.id, label: s.org_name ? `${s.name} (${s.org_name})` : s.name }))
	]);

	const form = createForm({
		initial: untrack(() => ({ site: pinned })),
		submit: async (v) => {
			await setDeviceSite(deviceId, v.site);
			const name = sites?.find((s) => s.id === v.site)?.name ?? '';
			say(v.site ? t('sites.device_pinned', { name }) : t('sites.device_unpinned'));
			devicesChanged(deviceId);
			onclose();
		}
	});
</script>

<Dialog title={t('sites.pin_title', { name: deviceName })} size="small" {onclose}>
	{#if loadError}
		<States kind="error" message={loadError} onretry={() => void load()} />
	{:else if !sites}
		<States kind="loading" message={t('sites.loading')} />
	{:else}
		<form id={form.id} onsubmit={form.onsubmit} novalidate>
			<Field label={t('sites.site')} hint={t('sites.pin_hint')}>
				{#snippet children(f)}
					<Select {...f} {options} bind:value={form.values.site} data-autofocus />
				{/snippet}
			</Field>
			<FormStatus status={form.status} />
		</form>
	{/if}
	{#snippet footer()}
		<Button onclick={onclose}>{t('actions.cancel')}</Button>
		<Button variant="primary" type="submit" form={form.id} busy={form.busy} disabled={!sites}
			>{t('actions.save')}</Button
		>
	{/snippet}
</Dialog>
