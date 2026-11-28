<!--
  Add a relay node: makes an enrollment string for the site as it opens
  and shows it with the install commands for Linux, Docker and Windows,
  each with a Copy button, and when the string expires. The string is in
  this answer only. While the dialog is open the page reloads the site, so
  a node that enrolls shows up here as connected.
-->
<script lang="ts">
	import { untrack } from 'svelte';
	import { ApiError } from '#lib/api/client.ts';
	import { createEnrollmentToken, type EnrollmentToken, type Site } from '#lib/api/sites.ts';
	import { formatDate } from '#lib/i18n/format.ts';
	import { t } from '#lib/i18n/i18n.svelte.ts';
	import Button from '#lib/ui/Button.svelte';
	import CodeBlock from '#lib/ui/CodeBlock.svelte';
	import Dialog from '#lib/ui/Dialog.svelte';
	import States from '#lib/ui/States.svelte';
	import Status from '#lib/ui/Status.svelte';
	import Tabs from '#lib/ui/Tabs.svelte';

	interface Props {
		site: Site;
		onclose: () => void;
	}

	let { site, onclose }: Props = $props();

	let token = $state.raw<EnrollmentToken | null>(null);
	let error = $state<string | null>(null);
	let platform = $state<'linux' | 'docker' | 'windows'>('linux');
	/** The nodes the site had when the dialog opened: any other one enrolled with this string */
	const before = untrack(() => new Set(site.nodes.map((n) => n.id)));
	const joined = $derived(site.nodes.find((n) => !before.has(n.id)) ?? null);

	async function make() {
		error = null;
		token = null;
		try {
			token = await createEnrollmentToken(site.id);
		} catch (err) {
			error = err instanceof ApiError || err instanceof Error ? err.message : t('sites.token_failed');
		}
	}

	$effect(() => {
		untrack(() => void make());
	});

	const tabs = $derived([
		{ id: 'linux', label: t('sites.install_linux') },
		{ id: 'docker', label: t('sites.install_docker') },
		{ id: 'windows', label: t('sites.install_windows') }
	]);
	const command = $derived(token ? token.commands[platform] || token.command : '');
	const commandNote = $derived(
		platform === 'docker'
			? t('sites.install_docker_note')
			: platform === 'windows'
				? t('sites.install_windows_note')
				: t('sites.install_linux_note')
	);
</script>

<Dialog title={t('sites.add_node_title', { name: site.name })} size="large" {onclose}>
	{#if error}
		<States kind="error" message={error} onretry={() => void make()} />
	{:else if !token}
		<States kind="loading" message={t('sites.token_making')} />
	{:else}
		<div class="node-dialog">
			<p class="intro">{t('sites.add_node_intro')}</p>
			<Tabs {tabs} bind:value={platform} label={t('sites.install_on')}>
				<p class="note">{commandNote}</p>
				<CodeBlock code={command} title={t('sites.install_command')} />
			</Tabs>
			<div class="join">
				<CodeBlock code={token.join} title={t('sites.join_string')} />
				<p class="note">
					{t('sites.join_expires', {
						date: formatDate(token.expires_at)
					})}
				</p>
			</div>
			<p class="wait" aria-live="polite">
				{#if joined}
					<Status
						tone={joined.online ? 'good' : 'offline'}
						label={t('sites.node_joined', { name: joined.name })}
					/>
				{:else}
					<Status tone="waiting" label={t('sites.node_waiting')} />
				{/if}
			</p>
		</div>
	{/if}
	{#snippet footer()}
		<Button variant={joined ? 'primary' : 'secondary'} onclick={onclose}>{t('actions.close')}</Button>
	{/snippet}
</Dialog>

<style>
	.intro {
		margin: 0 0 var(--space-3);
		max-width: var(--measure);
	}
	.note {
		margin: var(--space-2) 0;
		max-width: var(--measure);
		color: var(--text-muted);
		font-size: var(--text-sm);
	}
	.join {
		margin-top: var(--space-4);
	}
	.wait {
		margin: var(--space-3) 0 0;
	}
	/* Long strings wrap instead of scrolling a single line far out */
	.node-dialog :global(.code-block) {
		white-space: pre-wrap;
		overflow-wrap: anywhere;
	}
</style>
