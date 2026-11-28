<!--
  New site, or rename one: name, organization (new sites only, when the
  account may choose one) and VPN group. A short form, so a centred dialog.
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import { createSite, updateSite, type Site, type SiteOrg } from '#lib/api/sites.ts';
	import { t } from '#lib/i18n/i18n.svelte.ts';
	import Button from '#lib/ui/Button.svelte';
	import Dialog from '#lib/ui/Dialog.svelte';
	import Field from '#lib/ui/Field.svelte';
	import FormStatus from '#lib/ui/FormStatus.svelte';
	import Input from '#lib/ui/Input.svelte';
	import Select from '#lib/ui/Select.svelte';
	import { createForm } from '#lib/ui/form.svelte.ts';
	import { toast } from '#lib/ui/toasts.svelte.ts';

	interface Props {
		/** null: a new site */
		site: Site | null;
		organizations: SiteOrg[];
		/** The account may make sites of no organization */
		unassigned: boolean;
		onclose: () => void;
		onsaved: (site: Site) => void;
	}

	let { site, organizations, unassigned, onclose, onsaved }: Props = $props();

	const orgOptions = $derived([
		...(unassigned ? [{ value: '', label: t('sites.no_organization') }] : []),
		...organizations.map((o) => ({ value: o.id, label: o.name }))
	]);
	/** A choice only when there is more than one */
	const choosesOrg = $derived(!site && orgOptions.length > 1);

	const form = createForm({
		initial: untrack(() => ({
			name: site?.name ?? '',
			vpnGroup: site?.vpn_group ?? '',
			org: site?.org_id ?? (unassigned ? '' : (organizations[0]?.id ?? ''))
		})),
		validate: (v) => ({ name: v.name.trim() ? undefined : t('sites.name_required') }),
		submit: async (v) => {
			const saved = site
				? await updateSite(site.id, { name: v.name.trim(), vpn_group: v.vpnGroup.trim() })
				: await createSite({ name: v.name.trim(), vpn_group: v.vpnGroup.trim(), org_id: v.org });
			toast.success(
				site ? t('sites.site_saved', { name: saved.name }) : t('sites.site_created', { name: saved.name })
			);
			onsaved(saved);
		}
	});
</script>

<Dialog title={site ? t('sites.rename_title', { name: site.name }) : t('sites.add_site')} {onclose}>
	<form id={form.id} onsubmit={form.onsubmit} novalidate>
		<Field label={t('sites.name')} hint={t('sites.name_hint')} error={form.errors.name} required>
			{#snippet children(f)}
				<Input {...f} maxlength={100} autocomplete="off" data-autofocus bind:value={form.values.name} />
			{/snippet}
		</Field>
		{#if choosesOrg}
			<Field label={t('sites.organization')} hint={t('sites.organization_hint')} width="medium">
				{#snippet children(f)}
					<Select {...f} options={orgOptions} bind:value={form.values.org} />
				{/snippet}
			</Field>
		{/if}
		<Field label={t('sites.vpn_group')} hint={t('sites.vpn_group_hint')} width="medium">
			{#snippet children(f)}
				<Input
					{...f}
					maxlength={100}
					autocomplete="off"
					spellcheck={false}
					bind:value={form.values.vpnGroup}
				/>
			{/snippet}
		</Field>
		<FormStatus status={form.status} />
	</form>
	{#snippet footer()}
		<Button onclick={onclose}>{t('actions.cancel')}</Button>
		<Button variant="primary" type="submit" form={form.id} busy={form.busy}
			>{site ? t('actions.save') : t('sites.add_site')}</Button
		>
	{/snippet}
</Dialog>
