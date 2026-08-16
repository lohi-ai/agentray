'use client';

import { useMemo, useRef } from 'react';
import { CornerDownLeft, Paperclip } from 'lucide-react';
import {
  ChatComposer,
  ChatComposerInput,
  ChatComposerDrawer,
  type ChatComposerInputHandle,
  type ChatComposerTrigger,
} from '@astryxdesign/core/Chat';
import { createStaticSource, TypeaheadItem, type SearchableItem } from '@astryxdesign/core/Typeahead';
import { Button } from '@astryxdesign/core/Button';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { Token } from '@astryxdesign/core/Token';
import type { AgentChatCommand, AgentSkill } from '@/lib/api';
import { slugify, type Attachment } from './message-format';

// Auxiliary data carried on each `/` search item, so the dropdown can show a
// row's description without re-querying. `kind` separates the two things that
// share the `/` key: a COMMAND is an instruction to the runtime (run until this
// condition, compact this thread), a SKILL is an instruction to the agent for
// this turn. They read the same to a user typing `/`, so the menu says which is
// which rather than making them learn two prefixes.
type SlashAux = { description: string; kind: 'command' | 'skill' };

// Accepted attach types — kept in sync with isReadableFile() in message-format.ts.
// Used only as the native picker's `accept` hint; the real gate is at read time.
const ACCEPT = '.txt,.md,.mdx,.csv,.tsv,.json,.jsonl,.log,.yaml,.yml,.xml,.html,.htm,.css,.js,.jsx,.ts,.tsx,.py,.go,.rs,.java,.rb,.php,.sql,.sh,.bash,.toml,.ini,.env,.conf,.tf,.graphql,.svg,text/*';

export function Composer({
  value, onChange, onSubmit, onStop, isStopShown, placeholder, footerActions,
  skills, commands, attachments, onFiles, onRemoveAttachment, notice, steerMode, onSteerMode,
  headerContext,
}: {
  value: string;
  onChange: (v: string) => void;
  onSubmit: () => void;
  onStop: () => void;
  isStopShown: boolean;
  placeholder: string;
  footerActions: React.ReactNode;
  skills: AgentSkill[];
  // The server's slash-command catalog. Rendered above the skills in the same
  // `/` menu — they are the two things a slash can start, and hiding one behind
  // a different key would only mean the user has to know which is which before
  // they can look either up.
  commands: AgentChatCommand[];
  attachments: Attachment[];
  onFiles: (files: File[]) => void;
  onRemoveAttachment: (id: string) => void;
  notice?: string;
  // The quiet row above the input — capacity and other standing facts. Kept a
  // slot rather than a fixed control so the composer stays presentational.
  headerContext?: React.ReactNode;
  // While a turn streams, a send is an amendment to it rather than a new
  // question: 'steer' reaches the agent at its next turn boundary, 'followup'
  // runs once the current answer is finished. Only rendered when isStopShown.
  steerMode: 'steer' | 'followup';
  onSteerMode: (mode: 'steer' | 'followup') => void;
}) {
  const handleRef = useRef<ChatComposerInputHandle>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  // Only enabled+active skills are materialized into the system prompt, so those
  // are the only ones worth offering — invoking a proposed/disabled skill via /
  // would name a skill the runtime can't actually load.
  const usable = useMemo(() => skills.filter((s) => s.enabled && s.status === 'active'), [skills]);

  // One static typeahead source over both vocabularies; the item label is the
  // token the user types after `/`, so createStaticSource's default substring
  // match keys on the same string that ends up in the message.
  //
  // Commands come first: they are the shorter list, they are the same in every
  // workspace, and a user reaching for `/` with nothing typed is far more often
  // looking for one of six commands than for one of their skills.
  const source = useMemo(
    () => createStaticSource<SearchableItem<SlashAux>>([
      ...commands.map((c) => ({
        id: `cmd:${c.name}`,
        label: c.name,
        auxiliaryData: { description: c.arg ? `${c.summary} — takes ${c.arg}` : c.summary, kind: 'command' as const },
      })),
      ...usable.map((s) => ({
        id: s.id,
        label: slugify(s.name),
        auxiliaryData: { description: s.description, kind: 'skill' as const },
      })),
    ]),
    [commands, usable],
  );

  // Commands that take an argument are inserted as plain text with the caret left
  // after them, because the argument is the point: `/goal ` followed by whatever
  // the user is about to type. A chip would close the token and leave them typing
  // beside a command rather than into it. Everything else (bare commands, skills)
  // inserts as a chip, which is compact and unambiguous to re-read.
  const withArg = useMemo(
    () => new Set(commands.filter((c) => c.arg).map((c) => `cmd:${c.name}`)),
    [commands],
  );

  // The `/` trigger: open the menu, render each row with its description and a
  // kind marker, and insert either a chip or an argument-ready command line. The
  // serialized chip value is `/token`, which the send path parses back into a
  // skill directive — and which the server parses back into a command.
  const slashTrigger: ChatComposerTrigger = useMemo(() => ({
    character: '/',
    searchSource: source,
    menuLabel: 'Commands and skills',
    emptySearchResultsText: 'No matching command or skill',
    renderItem: (item) => {
      const aux = item.auxiliaryData as SlashAux | undefined;
      return <TypeaheadItem item={item} description={aux?.description} />;
    },
    onSelect: (item) => {
      const aux = item.auxiliaryData as SlashAux | undefined;
      // A plain string inserts as text, leaving the caret after it.
      if (withArg.has(item.id)) return `/${item.label} `;
      return {
        value: `/${item.label}`,
        label: `/${item.label}`,
        variant: aux?.kind === 'command' ? ('blue' as const) : ('purple' as const),
      };
    },
  }), [source, withArg]);

  const onPicked = (e: React.ChangeEvent<HTMLInputElement>) => {
    const list = e.target.files ? Array.from(e.target.files) : [];
    if (list.length) onFiles(list);
    e.target.value = ''; // reset so re-picking the same file fires onChange again
  };

  return (
    <>
      {/* Off-screen native picker driven by the paperclip button. Paste/drop go
          through ChatComposerInput.onFiles — both funnel into the same onFiles. */}
      <input ref={fileInputRef} type="file" multiple hidden accept={ACCEPT} onChange={onPicked} />
      <ChatComposer
        value={value}
        onChange={onChange}
        onSubmit={onSubmit}
        onStop={onStop}
        isStopShown={isStopShown}
        placeholder={placeholder}
        status={notice ? { type: 'warning', message: notice } : undefined}
        headerContext={headerContext}
        input={
          <ChatComposerInput
            handleRef={handleRef}
            value={value}
            onChange={onChange}
            onSubmit={onSubmit}
            placeholder={placeholder}
            triggers={commands.length || usable.length ? [slashTrigger] : []}
            onFiles={onFiles}
          />
        }
        drawer={attachments.length ? (
          <ChatComposerDrawer count={attachments.length} label="Attachments">
            {attachments.map((a) => (
              <Token
                key={a.id}
                label={a.name}
                size="sm"
                color="gray"
                icon={<Paperclip size={12} />}
                description={a.truncated ? 'Truncated to fit' : undefined}
                onRemove={() => onRemoveAttachment(a.id)}
              />
            ))}
          </ChatComposerDrawer>
        ) : undefined}
        footerActions={
          <>
            {/* Attach lives in the bottom action row, next to the agent picker. */}
            <Button
              isIconOnly
              variant="ghost"
              size="sm"
              label="Attach files"
              icon={<Paperclip size={16} />}
              onClick={() => fileInputRef.current?.click()}
            />
            {footerActions}
            {/* When to deliver an amendment. Not two send buttons — two adjacent
                equal-weight primary actions is exactly what the design system
                forbids — so the choice lives here and Steer stays the one
                button. Defaults to Now. */}
            {isStopShown ? (
              <SegmentedControl
                size="sm"
                label="When to deliver this message"
                value={steerMode}
                onChange={(v) => onSteerMode(v as 'steer' | 'followup')}
              >
                <SegmentedControlItem value="steer" label="Now" />
                <SegmentedControlItem value="followup" label="After this answer" />
              </SegmentedControl>
            ) : null}
          </>
        }
        // The primary button is Stop while streaming, so the send action moves
        // beside it with a visible text label (never icon-only). isDisabled is
        // deliberately never set while streaming: it puts pointer-events:none on
        // the whole composer root and would kill the Stop button's clicks too.
        sendActions={
          isStopShown ? (
            <Button
              size="sm"
              variant="secondary"
              label="Steer"
              icon={<CornerDownLeft size={14} />}
              onClick={onSubmit}
              isDisabled={!value.trim()}
            />
          ) : null
        }
      />
    </>
  );
}
