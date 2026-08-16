'use client';

// The composer's `/` vocabulary, client side.
//
// The server owns what a command MEANS (internal/runtime/chatcmd.go parses every
// message, whichever client sent it). This module owns only what the menu shows
// and how a typed line is rendered back in the transcript — and it fetches the
// catalog rather than hard-coding it, so a command added on the server appears
// here without a frontend release.
//
// FALLBACK_COMMANDS exists for the seconds before that fetch lands (and for an
// offline reload). It is a mirror, not a second source of truth: a stale entry
// here shows a menu row whose command the server would still parse correctly,
// because the server is the one parsing.

import { createContext, useContext } from 'react';
import { useQuery } from '@tanstack/react-query';
import { AgentRayAPI, type AgentChatCommand } from '@/lib/api';
import { useAuthStore } from '@/lib/app-state';

export const FALLBACK_COMMANDS: AgentChatCommand[] = [
  { name: 'goal', arg: '<condition>', summary: 'Keep working until a condition is met', handled: false },
  { name: 'plan', summary: "Show the agent's current plan for this thread", handled: true },
  { name: 'compact', summary: 'Summarize the older messages now to free up context', handled: true },
  { name: 'clear', summary: 'Start fresh from here — the agent forgets what came before', handled: true },
  { name: 'agents', summary: 'List the teammates this agent can hand work to', handled: true },
  { name: 'help', summary: 'List the commands and skills you can use here', handled: true },
];

// useChatCommands serves the catalog to the composer. It is workspace-independent
// and effectively static, so it is cached for the session rather than refetched
// per thread — this is a menu, not data.
export function useChatCommands() {
  const projectID = useAuthStore((s) => s.project?.id);
  const q = useQuery({
    queryKey: ['chat-commands'],
    queryFn: () => new AgentRayAPI(projectID!).listChatCommands(),
    enabled: !!projectID,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  });
  return { commands: q.data?.commands ?? FALLBACK_COMMANDS };
}

// CommandNames carries the catalog's names down to the transcript, which needs
// them only to decide whether a leading "/word" is a command chip or prose.
// A context rather than a prop chain: it is one immutable list, read three levels
// deep (Conversation → UserTurn → UserMessage), and threading it through every
// intermediate component would put a parameter on each of them that none of them
// has an opinion about.
export const CommandNames = createContext<string[]>(FALLBACK_COMMANDS.map((c) => c.name));

export function useCommandNames() {
  return useContext(CommandNames);
}

// parseCommandLine reads the leading slash command off a typed message, using the
// SAME rule the server does: first line only, first word only, and only a name in
// the catalog. Anything else is prose — a path, a fraction, a date — and must be
// left alone.
//
// The client parses at all for one reason: to render the user's bubble as the
// command it was, and to know when a turn will be answered instantly. It never
// decides what happens; that already happened on the server.
export function parseCommandLine(
  message: string,
  commands: AgentChatCommand[],
): { name: string; arg: string; rest: string } | null {
  const trimmed = message.trim();
  if (!trimmed.startsWith('/')) return null;
  const [line, ...restLines] = trimmed.split('\n');
  const [word, ...argWords] = line.trim().split(' ');
  const name = word.slice(1).toLowerCase();
  if (!commands.some((c) => c.name === name)) return null;
  return {
    name,
    arg: argWords.join(' ').trim().replace(/^["']|["']$/g, ''),
    rest: restLines.join('\n').trim(),
  };
}
