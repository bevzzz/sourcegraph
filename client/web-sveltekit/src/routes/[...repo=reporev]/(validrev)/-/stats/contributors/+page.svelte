<script lang="ts">
    import { goto } from '$app/navigation'
    import { page } from '$app/stores'
    import LoadingSpinner from '$lib/LoadingSpinner.svelte'
    import Paginator from '$lib/Paginator.svelte'
    import Timestamp from '$lib/Timestamp.svelte'
    import Avatar from '$lib/Avatar.svelte'
    import { createPromiseStore } from '$lib/utils'
    import { Button, ButtonGroup } from '$lib/wildcard'
    import type { ContributorConnection } from './page.gql'

    import type { PageData } from './$types'

    export let data: PageData

    const timePeriodButtons = [
        ['Last 7 days', '7 days ago'],
        ['Last 30 days', '30 days ago'],
        ['Last year', '1 year ago'],
        ['All time', ''],
    ]

    const { pending, latestValue: contributorConnection, set } = createPromiseStore<ContributorConnection | null>()
    $: set(data.contributors)

    // We want to show stale contributors data when the user navigates to
    // the next or previous page for the current time period. When the user
    // changes the time period we want to show a loading indicator instead.
    let currentContributorConnection = $contributorConnection
    $: if (!$pending && $contributorConnection) {
        currentContributorConnection = $contributorConnection
    }

    $: timePeriod = data.after

    async function setTimePeriod(event: MouseEvent) {
        const element = event.target as HTMLButtonElement
        timePeriod = element.dataset.value ?? ''
        const newURL = new URL($page.url)
        newURL.search = timePeriod ? `after=${timePeriod}` : ''
        // Don't show stale contributors when switching the time period
        currentContributorConnection = null
        await goto(newURL)
    }
</script>

<svelte:head>
    <title>Contributors - {data.displayRepoName} - Sourcegraph</title>
</svelte:head>

<section>
    <div class="root">
        <form method="GET">
            Time period: <input name="after" bind:value={timePeriod} placeholder="All time" />
            <ButtonGroup>
                {#each timePeriodButtons as [label, value]}
                    <Button variant="secondary">
                        <svelte:fragment slot="custom" let:buttonClass>
                            <button
                                class={buttonClass}
                                class:active={timePeriod === value}
                                type="button"
                                data-value={value}
                                on:click={setTimePeriod}>{label}</button
                            >
                        </svelte:fragment>
                    </Button>
                {/each}
            </ButtonGroup>
        </form>
        {#if !currentContributorConnection && $pending}
            <div class="mt-3">
                <LoadingSpinner />
            </div>
        {:else if currentContributorConnection}
            {@const nodes = currentContributorConnection.nodes}
            <table class="mt-3">
                <tbody>
                    {#each nodes as contributor}
                        {@const commit = contributor.commits.nodes[0]}
                        <tr>
                            <td
                                ><span><Avatar avatar={contributor.person} /></span>&nbsp;<span
                                    >{contributor.person.displayName}</span
                                ></td
                            >
                            <td
                                ><Timestamp date={new Date(commit.author.date)} strict />:
                                <a href={commit.canonicalURL}>{commit.subject}</a></td
                            >
                            <td>{contributor.count}&nbsp;commits</td>
                        </tr>
                    {/each}
                </tbody>
            </table>
            <div class="d-flex flex-column align-items-center">
                <Paginator disabled={$pending} pageInfo={currentContributorConnection.pageInfo} />
                <p class="mt-1 text-muted">
                    <small>Total contributors: {currentContributorConnection.totalCount}</small>
                </p>
            </div>
        {/if}
    </div>
</section>

<style lang="scss">
    section {
        overflow: auto;
        margin-top: 2rem;
    }

    div.root {
        max-width: 54rem;
        margin-left: auto;
        margin-right: auto;
        margin-bottom: 1rem;
    }

    table {
        border-collapse: collapse;
    }

    td {
        padding: 0.5rem;
        border-bottom: 1px solid var(--border-color);

        tr:last-child & {
            border-bottom: none;
        }

        span {
            white-space: nowrap;
            text-overflow: ellipsis;
        }
    }
</style>
