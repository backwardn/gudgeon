import React from 'react';
import { NavLink as Link } from "react-router-dom";
import Axios from 'axios';
import {
  DataList,
  DataListItem,
  DataListCell
} from '@patternfly/react-core';
import { LocaleNumber } from './helpers.js';

export class MetricsTopList extends React.Component {
  constructor(props) {
    super(props);
  };

  state = {
    data: [],
    timer: null
  };  

  updateData() {
    let { timer } = this.state;
    if ( timer != null ) {
      clearTimeout(timer)
    }

    let params = {};
    if ( this.props.limit && this.props.limit > 0 ) {
      params['limit'] = this.props.limit;
    }

    Axios
      .get("/api/metrics/top/" + this.props.topType, {
        params: params
      } )
      .then(response => {
        this.setState({ data: response.data });

        let newTimer = setTimeout(() => { this.updateData() },15000); // update every 15s

        // update the data in the state
        this.setState({ timer: newTimer })
      }).catch((error) => {
        let newTimer = setTimeout(() => { this.updateData() },60000); // on error try again in 60s

        // update the data in the state
        this.setState({ timer: newTimer })
      });
  }

  componentDidMount() {
    this.updateData()
  }  

  componentWillUnmount() {
    let { timer } = this.state;
    if ( timer != null ) {
      clearTimeout(timer)
    }
  }

  // maps the chosen type to the appropriate query key in the qlog page
  // (this is given as the st parameter)
  mapTypeToQuery = {
    "domains": "rdomain",
    "clients": "clientName",
    "rules": "matchRule"
  };

  render() {
    // build data list items from data
    let { data } = this.state;

    const ListItems = data.map((item, index) => {
      const doLink = this.mapTypeToQuery[this.props.topType] != null;
      return (
          <DataListItem key={ index } className={"smallListRow"} aria-labelledby={ "label-" + index }>
            <DataListCell className={"smallCell"} width={2}>
              <span className={"leftCard"} id={ "label-" + index }>
                { doLink ? <Link to={{ pathname: "/qlog", search: "?st=" + this.mapTypeToQuery[this.props.topType] + "&query=" + item.Desc }}>{ item.Desc }</Link> : item.Desc }
              </span>
            </DataListCell>
            <DataListCell className={"smallCell"} width={1}><div className={"rightCard"} >{ LocaleNumber(item.Count) }</div></DataListCell>
          </DataListItem>        
      );
    });

    return (
      <DataList aria-label={"Top " + this.props.topType}>
        {ListItems}
      </DataList>
    )
  }
}