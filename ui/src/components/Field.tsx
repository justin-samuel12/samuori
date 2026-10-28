import {
  Button as AriaButton,
  Checkbox as AriaCheckbox,
  FieldError,
  Input,
  Label,
  ListBox,
  ListBoxItem,
  Popover,
  Radio,
  RadioGroup as AriaRadioGroup,
  Select as AriaSelect,
  SelectValue,
  Text,
  TextArea,
  TextField as AriaTextField,
  type CheckboxProps,
  type Key,
  type TextFieldProps,
} from "react-aria-components";
import type { ReactNode } from "react";

interface TextProps extends TextFieldProps {
  label: string;
  description?: string;
  placeholder?: string;
  errorMessage?: string;
  /** A multi-line box, for pasted certificates and keys. */
  multiline?: boolean;
  /** Keeps the label for assistive technology only, for fields in list rows. */
  hideLabel?: boolean;
}

export function TextField({
  label,
  description,
  placeholder,
  errorMessage,
  multiline,
  hideLabel,
  ...props
}: TextProps) {
  return (
    <AriaTextField {...props} className="samuori-field">
      <Label className={hideLabel ? "sr-only" : undefined}>{label}</Label>
      {multiline ? (
        <TextArea placeholder={placeholder} rows={6} />
      ) : (
        <Input placeholder={placeholder} />
      )}
      {description && <Text slot="description">{description}</Text>}
      <FieldError>{errorMessage}</FieldError>
    </AriaTextField>
  );
}

export interface Option {
  id: string;
  label: string;
}

interface SelectProps {
  label: string;
  options: Option[];
  value: string | null;
  onChange: (v: string) => void;
  description?: string;
  isRequired?: boolean;
  name?: string;
  /** Keeps the label for assistive technology only, for selects inside table rows. */
  hideLabel?: boolean;
  placeholder?: string;
  isDisabled?: boolean;
}

export function Select({
  label,
  options,
  value,
  onChange,
  description,
  isRequired,
  name,
  hideLabel,
  placeholder,
  isDisabled,
}: SelectProps) {
  return (
    <AriaSelect
      className="samuori-field"
      value={value}
      onChange={(k: Key | null) => k !== null && onChange(String(k))}
      isRequired={isRequired}
      name={name}
      placeholder={placeholder}
      isDisabled={isDisabled}
    >
      <Label className={hideLabel ? "sr-only" : "samuori-field-label"}>
        {label}
      </Label>
      <AriaButton className="samuori-select-trigger">
        <SelectValue />
        <span aria-hidden="true">▾</span>
      </AriaButton>
      {description && <Text slot="description">{description}</Text>}
      <FieldError />
      <Popover className="samuori-popover">
        <ListBox className="samuori-listbox" items={options}>
          {(o) => (
            <ListBoxItem className="samuori-option">{o.label}</ListBoxItem>
          )}
        </ListBox>
      </Popover>
    </AriaSelect>
  );
}

export function Checkbox({
  children,
  ...props
}: CheckboxProps & { children: ReactNode }) {
  return (
    <AriaCheckbox {...props} className="samuori-checkbox">
      <span className="samuori-checkbox-box" aria-hidden="true" />
      {children}
    </AriaCheckbox>
  );
}

interface RadioProps {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: {
    id: string;
    label: string;
    description: string;
    isDisabled?: boolean;
  }[];
}

export function RadioGroup({ label, value, onChange, options }: RadioProps) {
  return (
    <AriaRadioGroup
      className="samuori-field samuori-radio-group"
      value={value}
      onChange={onChange}
    >
      <Label className="samuori-field-label">{label}</Label>
      {options.map((o) => (
        <Radio
          key={o.id}
          value={o.id}
          className="samuori-radio"
          isDisabled={o.isDisabled}
        >
          <span className="samuori-radio-dot" aria-hidden="true" />
          <span>
            <strong>{o.label}</strong>
            <span className="samuori-muted"> — {o.description}</span>
          </span>
        </Radio>
      ))}
    </AriaRadioGroup>
  );
}
