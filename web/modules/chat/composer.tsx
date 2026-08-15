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
import { HStack } from '@astryxdesign/core/HStack';
import { Token } from '@astryxdesign/core/Token';
import type { AgentSkill } from '@/lib/api';
import { slugify, type Attachment } from './message-format';

// Auxiliary data carried on each /command search item, so the dropdown can show a
// skill's description without re-querying.
type SkillAux = { description: string };

// Accepted attach types — kept in sync with isReadableFile() in message-format.ts.
// Used only as the native picker's `accept` hint; the real gate is at read time.
const ACCEPT = '.txt,.md,.mdx,.csv,.tsv,.json,.jsonl,.log,.yaml,.yml,.xml,.html,.htm,.css,.js,.jsx,.ts,.tsx,.py,.go,.rs,.java,.rb,.php,.sql,.sh,.bash,.toml,.ini,.env,.conf,.tf,.graphql,.svg,text/*';

export function Composer({
  value, onChange, onSubmit, onStop, isStopShown, placeholder, footerActions,
  skills, attachments, onFiles, onRemoveAttachment, notice, steerMode, onSteerMode,
}: {
  value: string;
  onChange: (v: string) => void;
  onSubmit: () => void;
  onStop: () => void;
  isStopShown: boolean;
  placeholder: string;
  footerActions: React.ReactNode;
  skills: AgentSkill[];
  attachments: Attachment[];
  onFiles: (files: File[]) => void;
  onRemoveAttachment: (id: string) => void;
  notice?: string;
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

  // A static typeahead source over the agent's skills; the item label is the slug
  // the user types after `/`, so createStaticSource's default substring match keys
  // on the same token that ends up in the message.
  const source = useMemo(
    () => createStaticSource<SearchableItem<SkillAux>>(
      usable.map((s) => ({ id: s.id, label: slugify(s.name), auxiliaryData: { description: s.description } })),
    ),
    [usable],
  );

  // The `/` trigger: open the skills menu, render each row with its description,
  // and insert a purple `/slug` chip whose serialized value the send path parses
  // back into a "Use your … skill." directive.
  const skillTrigger: ChatComposerTrigger = useMemo(() => ({
    character: '/',
    searchSource: source,
    menuLabel: 'Skills',
    emptySearchResultsText: 'No matching skills',
    renderItem: (item) => (
      <TypeaheadItem item={item} description={(item.auxiliaryData as SkillAux | undefined)?.description} />
    ),
    onSelect: (item) => ({ value: `/${item.label}`, label: `/${item.label}`, variant: 'purple' }),
  }), [source]);

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
        input={
          <ChatComposerInput
            handleRef={handleRef}
            value={value}
            onChange={onChange}
            onSubmit={onSubmit}
            placeholder={placeholder}
            triggers={usable.length ? [skillTrigger] : []}
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
              <HStack gap={0} align="center">
                <Button
                  size="sm"
                  variant={steerMode === 'steer' ? 'secondary' : 'ghost'}
                  label="Now"
                  onClick={() => onSteerMode('steer')}
                />
                <Button
                  size="sm"
                  variant={steerMode === 'followup' ? 'secondary' : 'ghost'}
                  label="After this answer"
                  onClick={() => onSteerMode('followup')}
                />
              </HStack>
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
