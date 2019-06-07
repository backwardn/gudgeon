import React from 'react';
import { NavLink as Link } from "react-router-dom";
import Axios from 'axios';
import { 
  Card,
  CardHeader,
  CardBody,
  Grid,
  GridItem,
  DataList,
  DataListItem,
  DataListCell,
  FormSelect,
  FormSelectOption,
  Split,
  SplitItem
} from '@patternfly/react-core';
import { 
  Table, 
  TableHeader, 
  TableBody, 
  TableVariant 
} from '@patternfly/react-table';
import { GudgeonChart } from './metrics-chart.js';
import { MetricsTopList } from './metrics-top.js';
import { HumanBytes, LocaleNumber } from './helpers.js';
import gudgeonStyles from '../../css/gudgeon-app.css';
import { css } from '@patternfly/react-styles';

export class Dashboard extends React.Component {
  constructor(props) {
    super(props);
  };

  ProcessorPercentFormatter = (value) => {
    return LocaleNumber(value / 1000) + "%"
  };

  chartMetrics = {
    "queries": {
      label: "Queries",
      formatter: LocaleNumber,
      series: {
        queries: { name: "Queries/s", key: "gudgeon-session-queries-ps" }, 
        blocked: { name: "Blocked/s", key: "gudgeon-session-blocks-ps" } 
      }
    },
    "memory": {
      label: "Memory",
      formatter: HumanBytes,
      series: {
        heap: { name: "Allocated Heap", key: "gudgeon-allocated-bytes" }, 
        rss: { name: "Resident Memory", key: "gudgeon-process-used-bytes" },
        cache: { name: "Cache Entries", key: "gudgeon-cache-entries", axis: "y2" } 
      }
    },
    "threads": {
      label: "Threads",
      formatter: LocaleNumber,
      series: { 
        threads: { name: "Threads", key: "gudgeon-process-threads" },
        routines: { name: "Go Routines", key: "gudgeon-goroutines" } 
      }
    },
    "cpu": {
      label: "CPU",
      formatter: this.ProcessorPercentFormatter,
      domain: {
        maxY: 100000, // processor use is in 1000ths of a percent
        minY: 0
      },
      ticks: [50000, 100000],
      series: { 
        cpu: { name: "CPU Use", key: "gudgeon-cpu-hundreds-percent" } 
      }
    }    
  };

  state = {
    width: 0,
    data: {
      'metrics': {
        'gudgeon-blocked-lifetime-queries': {
          'count': 0
        },
        'gudgeon-blocked-session-queries': {
          'count': 0
        },
        'gudgeon-total-lifetime-queries': {
          'count': 0
        },
        'gudgeon-total-session-queries': {
          'count': 0
        }
      },
      'lists': []
    },
    columns: [
      'List',
      'Rules',
      'Session Matches',
      'Lifetime Matches'
    ],
    currentMetrics: 'lifetime',
    rows: [],
    timer: null
  };  

  options = [
    { value: 'lifetime', label: 'Lifetime', disabled: false},
    { value: 'session', label: 'Session', disabled: false }
  ];

  onQueryMetricsOptionChange = (value, event) => {
    this.setState({ currentMetrics: value });
  };

  updateData() {
    // clear any old timers
    let { timer } = this.state;
    if ( timer != null ) {
      clearTimeout(timer)
    }

    Axios
      .get("/api/metrics/current")
      .then(response => {
        // set the state with the response data and then upate the card rows
        let rows = this.getResponseRows(response.data);
        this.setState({ rows: rows, data: response.data});
        
        let newTimer = setTimeout(() => { this.updateData() },2000); // update every 2s

        // update the data in the state
        this.setState({ timer: newTimer })
      }).catch((error) => {
        let newTimer = setTimeout(() => { this.updateData() },15000); // on error try again in 15s

        // update the data in the state
        this.setState({ timer: newTimer })
      });
  }

  getResponseRows(data) {
    // update the rows by building each
    let rows = [];
    data.lists.forEach((element) => {
      if ( element['name'] == null ) {
        return;
      }
      let newRow = [];
      newRow.push(element['name']);
      let key = element['short'];
      newRow.push(this.getDataMetric(data, 'rules-list-' + key));
      newRow.push(this.getDataMetric(data, 'rules-session-matched-' + key));
      newRow.push(this.getDataMetric(data, 'rules-lifetime-matched-' + key));
      rows.push(newRow);
    });

    return rows;
  }

  getDataMetric(data, key) {
    if ( data.metrics == null ) {
      return 0
    }
    if ( data.metrics["gudgeon-" + key] == null ) {
      return 0
    }
    return data.metrics["gudgeon-" + key].count;
  }

  componentDidMount() {
    // (safely) load state
    let stateString = localStorage.getItem("gudgeon-metrics-cards-state");
    if (stateString === "" || stateString == null) {
      stateString = "{}"
    }
    let savedState = JSON.parse(stateString);
    savedState['timer'] = null;

    // update data
    this.setState(savedState, this.updateData);
  }  

  componentWillUnmount() {
    // clear existing timer
    let { timer } = this.state;
    if ( timer != null ) {
      clearTimeout(timer)
    }

    // save state
    localStorage.setItem("gudgeon-metrics-cards-state", JSON.stringify(this.state));
  }
  
  render() {
    const { columns, data, rows, currentMetrics } = this.state;

    const topTypes = [
      { key: "clients", title: "Top Clients" },
      { key: "rules", title: "Top Rule Matches" },
      { key: "domains", title: "Top Queried Domains" }
    ];
    const TopCards = window.config().metrics_detailed ? (
      <React.Fragment>
        { topTypes.map((value, index) => {
          return (
              <GridItem key={index} lg={3} md={6} sm={12}>
                <Card className={css(gudgeonStyles.maxHeight)}>
                  <CardHeader>
                    <Split gutter="sm">
                      <SplitItem isFilled={true} style={{ width: "100%" }}>
                        { value.title }
                      </SplitItem>
                      <SplitItem isFilled={true} style={{ textAlign: "right" }}>
                        <Link to={ "/top/" + value.key } >more&gt;</Link>
                      </SplitItem>
                    </Split>
                  </CardHeader>
                  <CardBody>
                    <MetricsTopList topType={ value.key } />
                  </CardBody>
                </Card>
              </GridItem>
          );
        })}
      </React.Fragment>
    ) : null;

    const OverviewChart = window.config().metrics && window.config().metrics_persist ? (
      <GridItem lg={6} md={6} sm={12}>
        <Card className={css(gudgeonStyles.maxHeight)}>
          <CardBody>
            <GudgeonChart metrics={ this.chartMetrics } />
          </CardBody>
        </Card>
      </GridItem>
    ) : null;

    return (
      <Grid gutter="sm">
        <GridItem lg={3} md={6} sm={12}>
          <Card className={css(gudgeonStyles.maxHeight)}>
            <CardBody>
              <div style={{ "paddingBottom": "15px" }}>
                <FormSelect value={this.state.currentMetrics} onChange={this.onQueryMetricsOptionChange} aria-label="FormSelect Input">
                  {this.options.map((option, index) => (
                    <FormSelectOption isDisabled={option.disabled} key={index} value={option.value} label={option.label} />
                  ))}
                </FormSelect>
              </div>
              <DataList aria-label="Metrics">
                <DataListItem className={css(gudgeonStyles.smallListRow)} aria-labelledby="label-query">
                  <DataListCell width={2} className={css(gudgeonStyles.smallCell)}><span className={css(gudgeonStyles.leftCard)} id="label-query">Queries</span></DataListCell>
                  <DataListCell width={1} className={css(gudgeonStyles.smallCell)}><div className={css(gudgeonStyles.rightCard)}>{ LocaleNumber(this.getDataMetric(data, 'total-' + currentMetrics + '-queries')) }</div></DataListCell>
                </DataListItem>
                <DataListItem className={css(gudgeonStyles.smallListRow)} aria-labelledby="label-blocked">
                  <DataListCell width={2} className={css(gudgeonStyles.smallCell)}><span className={css(gudgeonStyles.leftCard)} id="label-blocked">Blocked</span></DataListCell>
                  <DataListCell width={1} className={css(gudgeonStyles.smallCell)}><div className={css(gudgeonStyles.rightCard)}>{ LocaleNumber(this.getDataMetric(data, 'blocked-' + currentMetrics + '-queries')) }</div></DataListCell>
                </DataListItem>
              </DataList>            
            </CardBody>
          </Card>          
        </GridItem>
        { TopCards }
        { OverviewChart }
        <GridItem lg={6} md={6} sm={12}>
          <Card className={css(gudgeonStyles.maxHeight)}>
            <CardBody>
              <Table aria-label="Lists" variant={TableVariant.compact} borders={false} cells={columns} rows={rows}>
                <TableHeader />
                <TableBody />
              </Table>            
            </CardBody>
          </Card>          
        </GridItem>
      </Grid>
    )
  }
}