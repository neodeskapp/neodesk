<!--
  A site's networks: one row per address range (CIDR and whether it is a
  private LAN range or a public address range), Add network, Remove, and
  one Save for the list. A refused entry is marked on its row with the
  server's reason. Read-only accounts get the list as text.
-->
<script lang="ts">
	import { ApiError } from '#lib/api/client.ts';
	import { saveNetworks, type Site } from '#lib/api/sites.ts';
	import { t, tf } from '#lib/i18n/i18n.svelte.ts';
	import {
		guessKind,
		networkRows,
		networksChanged,
		networksToSave,
		rowOfSentIndex,
		type NetworkRow
	} from '#lib/serverarea/sites.ts';
	import Button from '#lib/ui/Button.svelte';
	import Empty from '#lib/ui/Empty.svelte';
	import FormStatus from '#lib/ui/FormStatus.svelte';
	import Input from '#lib/ui/Input.svelte';
	import Select from '#lib/ui/Select.svelte';
	import Tag from '#lib/ui/Tag.svelte';
	import { ICONS } from '#lib/util/display.ts';

	interface Props {
		site: Site;
		editable: boolean;
		onsaved: () => void;
	}

	let { site, editable, onsaved }: Props = $props();

	const uid = $props.id();
	let rows = $state<NetworkRow[]>([]);
	let rowError = $state<{ index: number; message: string } | null>(null);
	let status = $state<{ state: 'idle' | 'saving' | 'saved' | 'error'; message: string }>({
		state: 'idle',
		message: ''
	});
	/** The site the rows were loaded from: another site, or new data of this one, resets them */
	let loadedFor = '';

	$effect(() => {
		const key = site.id + '|' + JSON.stringify(site.networks);
		if (key === loadedFor) return;
		loadedFor = key;
		rows = networkRows(site);
		rowError = null;
		status = { state: 'idle', message: '' };
	});

	const dirty = $derived(networksChanged(site, rows));
	const kindOptions = $derived([
		{ value: 'private', label: t('sites.kind_private') },
		{ value: 'public', label: t('sites.kind_public') }
	]);

	function add() {
		rows = [...rows, { cidr: '', kind: 'private' }];
		queueMicrotask(() => document.getElementById(`${uid}-cidr-${rows.length - 1}`)?.focus());
	}

	function remove(i: number) {
		rows = rows.filter((_, j) => j !== i);
		if (rowError?.index === i) rowError = null;
		queueMicrotask(() => document.getElementById(`${uid}-add`)?.focus());
	}

	/** A new address picks its kind once, as the person types it */
	function typed(i: number) {
		const row = rows[i];
		if (row && site.networks.every((n) => n.cidr !== row.cidr)) row.kind = guessKind(row.cidr);
	}

	async function save(event: SubmitEvent) {
		event.preventDefault();
		rowError = null;
		status = { state: 'saving', message: tf('ui_form.saving', 'Saving…') };
		try {
			await saveNetworks(site.id, networksToSave(rows));
			status = { state: 'saved', message: t('sites.networks_saved') };
			onsaved();
		} catch (err) {
			const message = err instanceof Error ? err.message : t('sites.networks_failed');
			const index =
				err instanceof ApiError && err.data && typeof err.data === 'object'
					? Number((err.data as Record<string, unknown>).index)
					: NaN;
			if (Number.isInteger(index)) {
				const row = rowOfSentIndex(rows, index);
				if (row >= 0) {
					rowError = { index: row, message };
					document.getElementById(`${uid}-cidr-${row}`)?.focus();
				}
			}
			status = { state: 'error', message };
		}
	}
</script>

{#if !editable}
	{#if site.networks.length}
		<ul class="net-list">
			{#each site.networks as n (n.cidr)}
				<li>
					<span class="mono">{n.cidr}</span>
					<Tag label={t(`sites.kind_${n.kind === 'public' ? 'public' : 'private'}`)} />
				</li>
			{/each}
		</ul>
	{:else}
		<p class="muted"><Empty word={t('sites.no_networks')} /></p>
	{/if}
{:else}
	<form class="net-form" onsubmit={save} novalidate aria-labelledby="{uid}-title">
		<span class="sr-only" id="{uid}-title">{t('sites.networks')}</span>
		{#if rows.length}
			<ul class="net-rows">
				{#each rows as row, i (i)}
					{@const invalid = rowError?.index === i}
					<li class="net-row">
						<span class="net-cidr">
							<label class="sr-only" for="{uid}-cidr-{i}">{t('sites.network_label', { n: i + 1 })}</label>
							<Input
								id="{uid}-cidr-{i}"
								bind:value={row.cidr}
								placeholder="10.1.0.0/16"
								autocomplete="off"
								spellcheck={false}
								class="mono"
								{invalid}
								describedby={invalid ? `${uid}-err` : undefined}
								oninput={() => typed(i)}
							/>
						</span>
						<span class="net-kind">
							<label class="sr-only" for="{uid}-kind-{i}">{t('sites.kind_label', { n: i + 1 })}</label>
							<Select id="{uid}-kind-{i}" options={kindOptions} bind:value={row.kind} />
						</span>
						<Button
							size="sm"
							variant="danger-quiet"
							aria-label={t('sites.remove_network', {
								cidr: row.cidr || t('sites.network_label', { n: i + 1 })
							})}
							onclick={() => remove(i)}>{t('actions.remove')}</Button
						>
						{#if invalid}<p class="net-error" id="{uid}-err">{rowError?.message}</p>{/if}
					</li>
				{/each}
			</ul>
		{:else}
			<p class="muted">{t('sites.no_networks_hint')}</p>
		{/if}
		<div class="net-actions">
			<Button id="{uid}-add" size="sm" icon={ICONS.add} onclick={add}>{t('sites.add_network')}</Button>
			<span class="net-save">
				<FormStatus {status} />
				<Button type="submit" size="sm" disabled={!dirty} busy={status.state === 'saving'}
					>{t('sites.save_networks')}</Button
				>
			</span>
		</div>
	</form>
{/if}

<style>
	.net-list,
	.net-rows {
		margin: 0;
		padding: 0;
		list-style: none;
	}
	.net-list > li {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		padding-block: var(--space-1);
	}
	.net-row {
		display: grid;
		grid-template-columns: minmax(10rem, 16rem) minmax(8rem, 10rem) auto;
		align-items: center;
		gap: var(--space-2);
		padding-block: var(--space-1);
	}
	.net-row > :global(.btn) {
		justify-self: start;
	}
	.net-error {
		grid-column: 1 / -1;
		margin: 0;
		color: var(--danger);
		font-size: var(--text-sm);
	}
	.net-actions {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
		margin-top: var(--space-2);
	}
	.net-save {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		margin-inline-start: auto;
	}
	.muted {
		margin: 0;
		color: var(--text-muted);
	}
	@media (max-width: 520px) {
		.net-row {
			grid-template-columns: minmax(0, 1fr) auto;
		}
		.net-cidr {
			grid-column: 1 / -1;
		}
	}
</style>
