<script lang="ts">
	import { onMount } from 'svelte';
	import { api, type ToolUse } from '#lib/api.ts';
	import { chat } from '#lib/chat.svelte.ts';
	import { toasts } from '#lib/toasts.svelte.ts';

	interface Props {
		/** The plan's opening line, shown first. It isn't a stored message. */
		opening: string;
	}
	let { opening }: Props = $props();

	const storageKey = 'whatnow.chat.open';
	let text = $state('');
	let log: HTMLElement | undefined = $state();

	// With no saved choice, the dock starts open on a wide screen and collapsed on a phone, where
	// it would cover the page.
	onMount(() => {
		let saved: string | null = null;
		try {
			saved = localStorage.getItem(storageKey);
		} catch {
			// Storage can be blocked; fall back to the screen size.
		}
		chat.open = saved === null ? !matchMedia('(max-width: 800px)').matches : saved === 'true';
	});

	function toggle() {
		chat.toggle();
		try {
			localStorage.setItem(storageKey, String(chat.open));
		} catch {
			// Not remembering is fine.
		}
	}

	$effect(() => {
		// Follow the conversation as it grows.
		void chat.messages.length;
		void chat.draft?.text;
		log?.scrollTo({ top: log.scrollHeight });
	});

	async function send(e: SubmitEvent) {
		e.preventDefault();
		const msg = text.trim();
		if (!msg || chat.busy) return;
		text = '';
		if (!(await chat.send(msg)) && !text) text = msg;
	}

	async function undo(batch: number) {
		try {
			await api.undo(batch);
		} catch (err) {
			toasts.error(err);
		}
	}

	const chipText = (t: ToolUse) => `${t.error ? '✕' : '✓'} ${t.label || t.tool}`;
</script>

{#if chat.open}
	<aside class="sticker dock">
		<header>
			<span class="label">Chat</span>
			<button class="collapse" aria-label="Collapse chat" onclick={toggle}>⟩</button>
		</header>
		<div class="log" bind:this={log}>
			{#if opening}
				<p class="bubble assistant">{opening}</p>
			{/if}
			{#each chat.messages as m (m.id)}
				<div class="bubble {m.role}" class:failed={m.error}>
					{#each m.tools ?? [] as t, i (i)}
						<span class="toolchip" class:bad={t.error} title={t.error}>{chipText(t)}</span>
					{/each}
					<p>{m.text}</p>
					{#if m.error}
						<a href="/settings">Check settings</a>
						<details class="small">
							<summary>What went wrong</summary>
							<code>{m.error}</code>
						</details>
					{/if}
					{#if m.batch_id}
						{#if chat.undone.includes(m.batch_id)}
							<span class="muted small">undone</span>
						{:else}
							<button class="btn small" onclick={() => undo(m.batch_id)}>Undo</button>
						{/if}
					{/if}
				</div>
			{/each}
			{#if chat.draft}
				<div class="bubble assistant">
					{#each chat.draft.tools as t, i (i)}
						<span class="toolchip" class:bad={t.error}>{chipText(t)}</span>
					{/each}
					<p>{chat.draft.text || '…'}</p>
				</div>
			{:else if chat.busy}
				<p class="bubble assistant">…</p>
			{/if}
		</div>
		<form onsubmit={send}>
			<input class="field" bind:value={text} placeholder="Talk to whatnow…" aria-label="Message" />
		</form>
		{#if chat.notice}
			<p class="notice muted">{chat.notice}</p>
		{/if}
	</aside>
{:else}
	<button class="sticker tab" aria-label="Open chat" onclick={toggle}>
		⟨ Chat
		{#if chat.unread || chat.busy}<span class="dot"></span>{/if}
	</button>
{/if}

<style>
	.dock {
		display: flex;
		flex-direction: column;
		width: 360px;
		height: calc(100vh - 32px);
		position: sticky;
		top: 16px;
		padding: 12px;
		gap: 10px;
	}
	header {
		display: flex;
		justify-content: space-between;
		align-items: center;
	}
	.collapse {
		background: none;
		border: none;
		font-size: 18px;
		font-weight: var(--heavy);
	}
	.log {
		flex: 1;
		overflow-y: auto;
		display: flex;
		flex-direction: column;
		gap: 8px;
	}
	.bubble {
		border-radius: var(--radius);
		padding: 8px 12px;
		max-width: 90%;
		margin: 0;
		display: flex;
		flex-direction: column;
		align-items: flex-start;
		gap: 4px;
	}
	.bubble p {
		margin: 0;
		white-space: pre-wrap;
	}
	.assistant {
		background: var(--paper-deep);
	}
	.user {
		background: var(--primary);
		color: var(--on-primary);
		align-self: flex-end;
	}
	.failed {
		background: var(--card);
		border: 2px dashed var(--ink-soft);
		color: var(--ink-soft);
	}
	details summary {
		cursor: pointer;
	}
	details code {
		display: block;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
		user-select: all;
	}
	.toolchip {
		font-size: 11px;
		font-weight: var(--heavy);
		background: var(--chip);
		border: 1.5px solid var(--ink);
		border-radius: var(--pill);
		padding: 0 8px;
	}
	.toolchip.bad {
		background: var(--must);
	}
	.btn.small {
		font-size: 12px;
		padding: 2px 8px;
	}
	.small {
		font-size: 12px;
	}
	.notice {
		margin: 0;
		font-size: 13px;
	}
	.tab {
		position: fixed;
		right: 0;
		top: 40%;
		writing-mode: vertical-rl;
		padding: 14px 6px;
		border-radius: var(--radius) 0 0 var(--radius);
		font-weight: var(--heavy);
		background: var(--primary);
		color: var(--on-primary);
	}
	.dot {
		display: inline-block;
		width: 10px;
		height: 10px;
		margin-top: 6px;
		border-radius: 50%;
		background: var(--now);
		border: 2px solid var(--ink);
	}

	/* Narrow screens: the dock is a bottom sheet, and the tab is a bar along the bottom. */
	@media (max-width: 800px) {
		.dock {
			position: fixed;
			left: 0;
			right: 0;
			bottom: 0;
			top: auto;
			width: auto;
			height: 65vh;
			border-radius: var(--radius) var(--radius) 0 0;
			z-index: 20;
		}
		.tab {
			top: auto;
			bottom: 0;
			left: 0;
			right: 0;
			writing-mode: horizontal-tb;
			border-radius: var(--radius) var(--radius) 0 0;
		}
	}
</style>
